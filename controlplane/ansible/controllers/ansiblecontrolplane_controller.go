package controllers

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/controllers/external"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	clusterkubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	clustersecret "sigs.k8s.io/cluster-api/util/secret"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// AnsibleControlPlaneReconciler reconciles an AnsibleControlPlane object.
type AnsibleControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	ansibleControlPlaneKind = "AnsibleControlPlane"

	initLockNameSuffix        = "init-lock"
	initLockLeaseDurationSecs = int32(600)

	etcdMachineLabel = "controlplane.cluster.x-k8s.io/etcd-machine"
)

// Reconcile implements the main reconciliation loop.
func (r *AnsibleControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	acp := &controlplanev1alpha1.AnsibleControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, acp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(acp, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		patchErr := patchHelper.Patch(ctx, acp,
			patch.WithStatusObservedGeneration{},
			patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
				clusterv1.ReadyCondition,
				controlplanev1alpha1.CertificatesAvailableCondition,
				controlplanev1alpha1.KubeconfigAvailableCondition,
				controlplanev1alpha1.MachinesCreatedCondition,
				controlplanev1alpha1.PostBootstrapReadyCondition,
			}},
		)
		if patchErr != nil {
			logger.Error(patchErr, "failed to patch ACP status")
		}
	}()

	cluster, err := util.GetOwnerCluster(ctx, r.Client, acp.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("waiting for owner Cluster to be set")
		return ctrl.Result{}, nil
	}

	if !acp.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, acp)
	}

	// Ensure finalizer is present so future cleanups can proceed.
	if !controllerutil.ContainsFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer) {
		controllerutil.AddFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer)
		return ctrl.Result{}, nil
	}

	if !cluster.Status.InfrastructureReady {
		msg := "Cluster infrastructure is not ready yet"
		conditions.MarkFalse(acp, controlplanev1alpha1.CertificatesAvailableCondition, controlplanev1alpha1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, msg)
		conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, msg)
		conditions.MarkFalse(acp, clusterv1.ReadyCondition, controlplanev1alpha1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, msg)
		return ctrl.Result{}, nil
	}

	if err := r.reconcileClusterCertificates(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		msg := "Cluster does not yet have a ControlPlaneEndpoint defined"
		conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.WaitingForControlPlaneEndpointReason, clusterv1.ConditionSeverityInfo, msg)
		conditions.MarkFalse(acp, controlplanev1alpha1.PostBootstrapReadyCondition, controlplanev1alpha1.WaitingForControlPlaneEndpointReason, clusterv1.ConditionSeverityInfo, msg)
		return ctrl.Result{}, nil
	}

	if err := r.syncMachines(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	machineList, err := r.listControlPlaneMachines(ctx, acp, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	var primary *clusterv1.Machine
	if len(machineList.Items) > 0 {
		primary = selectPrimaryMachine(machineList.Items, acp.Status.ControlPlaneInitialMachine)
	}
	configCache := ansibleConfigCache{}
	postBootstrapReady, nodeReady, err := r.primaryMachineProgress(ctx, primary, configCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.handlePrimaryState(ctx, acp, cluster, primary, postBootstrapReady, nodeReady); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureBootstrapAnchors(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if acp.Status.CurrentVersion == "" {
		acp.Status.CurrentVersion = acp.Spec.Version
	} else if acp.Status.CurrentVersion != acp.Spec.Version {
		msg := fmt.Sprintf("changing spec.version from %s to %s is not supported yet", acp.Status.CurrentVersion, acp.Spec.Version)
		acp.Status.FailureReason = controlplanev1alpha1.UpgradeUnsupportedReason
		acp.Status.FailureMessage = ptr.To(msg)
		conditions.MarkFalse(acp, clusterv1.ReadyCondition, controlplanev1alpha1.UpgradeUnsupportedReason, clusterv1.ConditionSeverityError, msg)
		logger.Error(fmt.Errorf(msg), "spec.version changes are not yet implemented")
		return ctrl.Result{}, nil
	} else {
		acp.Status.FailureReason = ""
		acp.Status.FailureMessage = nil
	}

	if err := r.updateReplicaStatus(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	conditions.SetSummary(acp,
		conditions.WithConditions(
			controlplanev1alpha1.CertificatesAvailableCondition,
			controlplanev1alpha1.KubeconfigAvailableCondition,
			controlplanev1alpha1.MachinesCreatedCondition,
			clusterv1.MachinesReadyCondition,
			controlplanev1alpha1.PostBootstrapReadyCondition,
		),
	)

	logger.V(2).Info("reconciled AnsibleControlPlane", "observedVersion", acp.Status.CurrentVersion)

	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller into the manager.
func (r *AnsibleControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1alpha1.AnsibleControlPlane{}).
		Owns(&clusterv1.Machine{}).
		Owns(&bootstrapv1.AnsibleConfig{}).
		Complete(r)
}

func (r *AnsibleControlPlaneReconciler) updateReplicaStatus(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	selectorLabels := labels.Set{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}
	acp.Status.Selector = selectorLabels.AsSelector().String()

	machineList := &clusterv1.MachineList{}
	listOpts := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels(selectorLabels),
	}
	if err := r.Client.List(ctx, machineList, listOpts...); err != nil {
		return err
	}

	machineCollection := collections.FromMachineList(machineList)
	conditions.SetAggregate(acp, clusterv1.MachinesReadyCondition, machineCollection.ConditionGetters(), conditions.AddSourceRef())

	var readyCount int32
	for i := range machineList.Items {
		if conditions.IsTrue(&machineList.Items[i], clusterv1.ReadyCondition) {
			readyCount++
		}
	}

	replicas := int32(len(machineList.Items))
	acp.Status.Replicas = replicas
	acp.Status.UpdatedReplicas = replicas
	acp.Status.ReadyReplicas = readyCount
	acp.Status.AvailableReplicas = readyCount
	unavailable := replicas - readyCount
	if unavailable < 0 {
		unavailable = 0
	}
	acp.Status.UnavailableReplicas = unavailable

	if acp.Status.CurrentVersion != "" {
		acp.Status.Version = ptr.To(acp.Status.CurrentVersion)
	} else {
		acp.Status.Version = nil
	}

	acp.Status.Initialized = acp.Status.ControlPlaneInitialMachine != nil
	acp.Status.Ready = conditions.IsTrue(acp, clusterv1.ReadyCondition)
	return nil
}

func (r *AnsibleControlPlaneReconciler) reconcileDelete(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer) {
		controllerutil.RemoveFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer)
	}
	return ctrl.Result{}, nil
}

func (r *AnsibleControlPlaneReconciler) reconcileClusterCertificates(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	controllerRef := metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind))
	certificates := clustersecret.NewCertificatesForInitialControlPlane(nil)
	if err := certificates.LookupOrGenerate(ctx, r.Client, util.ObjectKey(cluster), *controllerRef); err != nil {
		conditions.MarkFalse(acp, controlplanev1alpha1.CertificatesAvailableCondition, controlplanev1alpha1.CertificatesGenerationFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		return err
	}
	conditions.MarkTrue(acp, controlplanev1alpha1.CertificatesAvailableCondition)
	return nil
}

func (r *AnsibleControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	secretKey := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      clustersecret.Name(cluster.Name, clustersecret.Kubeconfig),
	}
	existing := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretKey, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.KubeconfigGenerationFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
			return err
		}
		owner := metav1.NewControllerRef(cluster, clusterv1.GroupVersion.WithKind("Cluster"))
		if err := clusterkubeconfig.CreateSecretWithOwner(ctx, r.Client, util.ObjectKey(cluster), cluster.Spec.ControlPlaneEndpoint.String(), *owner); err != nil {
			conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.KubeconfigGenerationFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
			return err
		}
	} else {
		if err := r.ensureKubeconfigMetadata(ctx, cluster, existing); err != nil {
			conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.KubeconfigGenerationFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
			return err
		}
	}
	conditions.MarkTrue(acp, controlplanev1alpha1.KubeconfigAvailableCondition)
	return nil
}

func (r *AnsibleControlPlaneReconciler) ensureKubeconfigMetadata(ctx context.Context, cluster *clusterv1.Cluster, cfgSecret *corev1.Secret) error {
	updated := false
	if cfgSecret.Labels == nil {
		cfgSecret.Labels = map[string]string{}
	}
	if cfgSecret.Labels[clusterv1.ClusterNameLabel] != cluster.Name {
		cfgSecret.Labels[clusterv1.ClusterNameLabel] = cluster.Name
		updated = true
	}
	if cfgSecret.Type != clusterv1.ClusterSecretType {
		cfgSecret.Type = clusterv1.ClusterSecretType
		updated = true
	}
	clusterOwner := metav1.NewControllerRef(cluster, clusterv1.GroupVersion.WithKind("Cluster"))
	newRefs := util.EnsureOwnerRef(cfgSecret.GetOwnerReferences(), *clusterOwner)
	if len(newRefs) != len(cfgSecret.GetOwnerReferences()) {
		cfgSecret.SetOwnerReferences(newRefs)
		updated = true
	}
	if updated {
		return r.Client.Update(ctx, cfgSecret)
	}
	return nil
}

func (r *AnsibleControlPlaneReconciler) syncMachines(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	controlPlaneDesired := int32(1)
	if acp.Spec.Replicas != nil {
		controlPlaneDesired = *acp.Spec.Replicas
	}
	isEtcdRequired := desiredEtcdReplicas(acp) > 0
	if controlPlaneDesired <= 0 && !isEtcdRequired {
		conditions.MarkTrue(acp, controlplanev1alpha1.MachinesCreatedCondition)
		return nil
	}

	machineList, err := r.listControlPlaneMachines(ctx, acp, cluster)
	if err != nil {
		return err
	}

	current := int32(len(machineList.Items))
	if current > 0 && current < controlPlaneDesired {
		primary := selectPrimaryMachine(machineList.Items, acp.Status.ControlPlaneInitialMachine)
		cache := ansibleConfigCache{}
		postBootstrapReady, nodeReady, err := r.primaryMachineProgress(ctx, primary, cache)
		if err != nil {
			return err
		}
		if !postBootstrapReady || !nodeReady {
			if primary != nil {
				if err := r.holdInitLease(ctx, acp, primary.Name); err != nil {
					return err
				}
			}
			msg := "waiting for the first control plane Machine to finish initialization before creating additional replicas"
			conditions.MarkFalse(acp, controlplanev1alpha1.MachinesCreatedCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, msg)
			return nil
		}
	}

	if current >= controlPlaneDesired {
		conditions.MarkTrue(acp, controlplanev1alpha1.MachinesCreatedCondition)
		return r.ensureStandaloneEtcdMachines(ctx, acp, cluster)
	}

	for current < controlPlaneDesired {
		reason, err := r.createControlPlaneMachine(ctx, acp, cluster)
		if err != nil {
			if reason == "" {
				reason = controlplanev1alpha1.MachineCreationFailedReason
			}
			conditions.MarkFalse(acp, controlplanev1alpha1.MachinesCreatedCondition, reason, clusterv1.ConditionSeverityError, err.Error())
			return err
		}
		current++
	}

	conditions.MarkTrue(acp, controlplanev1alpha1.MachinesCreatedCondition)
	return r.ensureStandaloneEtcdMachines(ctx, acp, cluster)
}

func (r *AnsibleControlPlaneReconciler) createControlPlaneMachine(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) (string, error) {
	machineName := names.SimpleNameGenerator.GenerateName(acp.Name + "-")
	labels := buildMachineLabels(acp, cluster)
	annotations := buildMachineAnnotations(acp)

	infraRef, err := r.cloneInfrastructureMachine(ctx, acp, cluster, machineName, labels, annotations)
	if err != nil {
		return controlplanev1alpha1.InfrastructureTemplateCloneFailedReason, err
	}

	bootstrapRef, err := r.createBootstrapConfig(ctx, acp, cluster, machineName, labels, annotations, nil)
	if err != nil {
		return controlplanev1alpha1.BootstrapConfigCreationFailedReason, err
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: acp.Namespace,
			Labels:    labels,
			Annotations: func() map[string]string {
				result := map[string]string{}
				for k, v := range annotations {
					result[k] = v
				}
				return result
			}(),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind)),
			},
		},
		Spec: buildMachineSpec(acp, cluster, infraRef, bootstrapRef),
	}

	if err := r.Client.Create(ctx, machine); err != nil {
		return controlplanev1alpha1.MachineCreationFailedReason, err
	}

	if err := r.adoptBootstrapConfig(ctx, bootstrapRef, machine); err != nil {
		return controlplanev1alpha1.BootstrapConfigCreationFailedReason, err
	}

	return "", nil
}

func (r *AnsibleControlPlaneReconciler) createEtcdMachine(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	machineName := names.SimpleNameGenerator.GenerateName(acp.Name + "-etcd-")
	labels := buildEtcdMachineLabels(acp, cluster)
	annotations := buildMachineAnnotations(acp)

	infraRef, err := r.cloneInfrastructureMachine(ctx, acp, cluster, machineName, labels, annotations)
	if err != nil {
		return err
	}

	bootstrapRef, err := r.createBootstrapConfig(ctx, acp, cluster, machineName, labels, annotations, []string{"etcd"})
	if err != nil {
		return err
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: acp.Namespace,
			Labels:    labels,
			Annotations: func() map[string]string {
				result := map[string]string{}
				for k, v := range annotations {
					result[k] = v
				}
				return result
			}(),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind)),
			},
		},
		Spec: buildMachineSpec(acp, cluster, infraRef, bootstrapRef),
	}

	if err := r.Client.Create(ctx, machine); err != nil {
		return err
	}
	return r.adoptBootstrapConfig(ctx, bootstrapRef, machine)
}

func (r *AnsibleControlPlaneReconciler) ensureStandaloneEtcdMachines(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	desired := desiredEtcdReplicas(acp)
	if desired <= 0 {
		return nil
	}
	etcdList, err := r.listEtcdMachines(ctx, acp, cluster)
	if err != nil {
		return err
	}
	cache := ansibleConfigCache{}
	for i := range etcdList.Items {
		if err := r.ensureMachineRole(ctx, &etcdList.Items[i], cache, "etcd"); err != nil {
			return err
		}
	}
	current := int32(len(etcdList.Items))
	for current < desired {
		if err := r.createEtcdMachine(ctx, acp, cluster); err != nil {
			return err
		}
		current++
	}
	return nil
}

func (r *AnsibleControlPlaneReconciler) cloneInfrastructureMachine(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, name string, labels, annotations map[string]string) (*corev1.ObjectReference, error) {
	templateRef := acp.Spec.MachineTemplate.Spec.InfrastructureRef.DeepCopy()
	if templateRef.Namespace == "" {
		templateRef.Namespace = acp.Namespace
	}
	owner := metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind))
	return external.CreateFromTemplate(ctx, &external.CreateFromTemplateInput{
		Client:      r.Client,
		TemplateRef: templateRef,
		Namespace:   acp.Namespace,
		Name:        name,
		ClusterName: cluster.Name,
		OwnerRef:    owner,
		Labels:      labels,
		Annotations: annotations,
	})
}

func (r *AnsibleControlPlaneReconciler) createBootstrapConfig(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, machineName string, labels, annotations map[string]string, roleOverride []string) (*corev1.ObjectReference, error) {
	specCopy := acp.Spec.AnsibleConfigSpec.DeepCopy()
	if len(roleOverride) > 0 {
		specCopy.Role = make([]string, len(roleOverride))
		copy(specCopy.Role, roleOverride)
	}
	cfg := &bootstrapv1.AnsibleConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-config", machineName),
			Namespace: acp.Namespace,
			Labels: func() map[string]string {
				out := map[string]string{}
				for k, v := range labels {
					out[k] = v
				}
				return out
			}(),
			Annotations: func() map[string]string {
				out := map[string]string{}
				for k, v := range annotations {
					out[k] = v
				}
				return out
			}(),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind)),
			},
		},
		Spec: *specCopy,
	}
	cfg.Labels[clusterv1.ClusterNameLabel] = cluster.Name

	if err := r.Client.Create(ctx, cfg); err != nil {
		return nil, err
	}

	return &corev1.ObjectReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "AnsibleConfig",
		Name:       cfg.Name,
		Namespace:  cfg.Namespace,
		UID:        cfg.UID,
	}, nil
}

func (r *AnsibleControlPlaneReconciler) adoptBootstrapConfig(ctx context.Context, ref *corev1.ObjectReference, machine *clusterv1.Machine) error {
	if ref == nil || ref.Namespace == "" {
		return nil
	}
	cfg := &bootstrapv1.AnsibleConfig{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, cfg); err != nil {
		return err
	}
	owner := metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Machine",
		Name:       machine.Name,
		UID:        machine.UID,
	}
	cfg.SetOwnerReferences(util.EnsureOwnerRef(cfg.GetOwnerReferences(), owner))
	return r.Client.Update(ctx, cfg)
}

func (r *AnsibleControlPlaneReconciler) holdInitLease(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, holder string) error {
	if holder == "" {
		return nil
	}
	key := client.ObjectKey{Namespace: acp.Namespace, Name: initLockName(acp)}
	now := metav1.MicroTime{Time: time.Now()}
	lease := &coordinationv1.Lease{}
	if err := r.Client.Get(ctx, key, lease); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      key.Name,
				Namespace: key.Namespace,
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: acp.Labels[clusterv1.ClusterNameLabel],
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind)),
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				AcquireTime:          &now,
				RenewTime:            &now,
				LeaseDurationSeconds: ptr.To(initLockLeaseDurationSecs),
			},
		}
		if lease.Labels[clusterv1.ClusterNameLabel] == "" && acp.Labels[clusterv1.ClusterNameLabel] != "" {
			lease.Labels[clusterv1.ClusterNameLabel] = acp.Labels[clusterv1.ClusterNameLabel]
		}
		return r.Client.Create(ctx, lease)
	}
	lease.Spec.HolderIdentity = ptr.To(holder)
	lease.Spec.RenewTime = &now
	if lease.Spec.AcquireTime == nil {
		lease.Spec.AcquireTime = &now
	}
	lease.Spec.LeaseDurationSeconds = ptr.To(initLockLeaseDurationSecs)
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	if lease.Labels[clusterv1.ClusterNameLabel] == "" && acp.Labels[clusterv1.ClusterNameLabel] != "" {
		lease.Labels[clusterv1.ClusterNameLabel] = acp.Labels[clusterv1.ClusterNameLabel]
	}
	return r.Client.Update(ctx, lease)
}

func (r *AnsibleControlPlaneReconciler) releaseInitLease(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane) error {
	key := client.ObjectKey{Namespace: acp.Namespace, Name: initLockName(acp)}
	lease := &coordinationv1.Lease{}
	if err := r.Client.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return r.Client.Delete(ctx, lease)
}
