package controllers

import (
	"context"
	"fmt"
	"time"

	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/controllers/external"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
	workloadprobe "sigs.k8s.io/cluster-api/controlplane/ansible/internal/workloadprobe"
	"sigs.k8s.io/cluster-api/internal/ansiblecontract"
	"sigs.k8s.io/cluster-api/internal/contract"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	clusterkubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	clustersecret "sigs.k8s.io/cluster-api/util/secret"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// AnsibleControlPlaneReconciler reconciles an AnsibleControlPlane object.
type AnsibleControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Probe  workloadprobe.WorkloadProbe
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
			patch.WithOwnedConditions{Conditions: []string{
				string(clusterv1.ReadyCondition),
				string(controlplanev1alpha1.CertificatesAvailableCondition),
				string(controlplanev1alpha1.KubeconfigAvailableCondition),
				string(controlplanev1alpha1.MachinesCreatedCondition),
				string(controlplanev1alpha1.PostBootstrapReadyCondition),
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
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !acp.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, acp)
	}

	// Ensure finalizer is present so future cleanups can proceed.
	if !controllerutil.ContainsFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer) {
		controllerutil.AddFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer)
		return ctrl.Result{}, nil
	}

	if !conditions.IsTrue(cluster, string(clusterv1.ClusterInfrastructureReadyCondition)) {
		msg := "Cluster infrastructure is not ready yet"
		setACPCondition(acp, controlplanev1alpha1.CertificatesAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		setACPCondition(acp, clusterv1.ReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 追加等待：在基础设施就绪后，确保 OpenStackCluster 的 APIServerLoadBalancer 就绪且 Bastion 处于 ACTIVE。
	// 若不满足，仍以“基础设施未就绪”对外呈现，复用现有等待原因与消息。
	if ok, err := r.ensureOpenStackInfraStatusReady(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	} else if !ok {
		msg := "Cluster infrastructure bastion is not ready yet"
		setACPCondition(acp, controlplanev1alpha1.CertificatesAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		setACPCondition(acp, clusterv1.ReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForClusterInfrastructureReason, msg)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.reconcileClusterCertificates(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if !cluster.Spec.ControlPlaneEndpoint.IsValid() {
		msg := "Cluster does not yet have a ControlPlaneEndpoint defined"
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForControlPlaneEndpointReason, msg)
		setACPCondition(acp, controlplanev1alpha1.PostBootstrapReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForControlPlaneEndpointReason, msg)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Generate or reconcile the workload cluster kubeconfig as soon as
	// certificates are available and the ControlPlaneEndpoint is valid.
	// This intentionally avoids waiting for Node readiness, aligning with KCP.
	if err := r.reconcileKubeconfig(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
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

	if err := r.syncAnchorAnnotations(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	if acp.Status.CurrentVersion == "" {
		acp.Status.CurrentVersion = acp.Spec.Version
	} else if acp.Status.CurrentVersion != acp.Spec.Version {
		msg := fmt.Sprintf("changing spec.version from %s to %s is not supported yet", acp.Status.CurrentVersion, acp.Spec.Version)
		acp.Status.FailureReason = controlplanev1alpha1.UpgradeUnsupportedReason
		acp.Status.FailureMessage = ptr.To(msg)
		setACPCondition(acp, clusterv1.ReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.UpgradeUnsupportedReason, msg)
		logger.Error(fmt.Errorf("%s", msg), "spec.version changes are not yet implemented")
		return ctrl.Result{}, nil
	} else {
		acp.Status.FailureReason = ""
		acp.Status.FailureMessage = nil
	}

	if err := r.updateReplicaStatus(ctx, acp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Surface v1beta2-style controlPlaneInitialized based on remote API liveness (/livez).
	if ptr := acp.Status.Initialization.ControlPlaneInitialized; ptr == nil || !*ptr {
		if ok, _ := r.Probe.IsInitialized(ctx, r.Client, cluster); ok {
			t := true
			acp.Status.Initialization.ControlPlaneInitialized = &t
		}
	}

	if err := conditions.SetSummaryCondition(acp, acp, string(clusterv1.ReadyCondition),
		conditions.ForConditionTypes{
			string(controlplanev1alpha1.CertificatesAvailableCondition),
			string(controlplanev1alpha1.KubeconfigAvailableCondition),
			string(controlplanev1alpha1.MachinesCreatedCondition),
			string(clusterv1.MachinesReadyCondition),
			string(controlplanev1alpha1.PostBootstrapReadyCondition),
		},
	); err != nil {
		logger.Error(err, "failed to summarize Ready condition")
	}

	logger.V(2).Info("reconciled AnsibleControlPlane", "observedVersion", acp.Status.CurrentVersion)

	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller into the manager.
func (r *AnsibleControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Probe == nil {
		r.Probe = workloadprobe.NewLivezProbe(0)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1alpha1.AnsibleControlPlane{}).
		Owns(&clusterv1.Machine{}).
		Owns(&bootstrapv1.AnsibleConfig{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=openstackclusters,verbs=get;list;watch

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
	if machineCollection.Len() > 0 {
		if err := conditions.SetAggregateCondition(machineCollection.UnsortedList(), acp, string(clusterv1.MachinesReadyCondition)); err != nil {
			return err
		}
	} else {
		conditions.Delete(acp, string(clusterv1.MachinesReadyCondition))
	}

	var readyCount int32
	for i := range machineList.Items {
		if conditions.IsTrue(&machineList.Items[i], string(clusterv1.ReadyCondition)) {
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
	acp.Status.Ready = conditions.IsTrue(acp, string(clusterv1.ReadyCondition))
	return nil
}

func (r *AnsibleControlPlaneReconciler) reconcileDelete(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer) {
		controllerutil.RemoveFinalizer(acp, controlplanev1alpha1.AnsibleControlPlaneFinalizer)
	}
	return ctrl.Result{}, nil
}

// ensureOpenStackInfraStatusReady 检查 OpenStackCluster 的关键状态：
// 1) APIServerLoadBalancer 已分配内网 VIP（internalIP 非空）；
// 2) 若启用了 bastion（spec.bastion.enabled=true），则 bastion.status.state 必须为 ACTIVE。
// 返回 (true,nil) 表示满足条件；(false,nil) 表示需要继续等待；出错返回 (false,err)。
func (r *AnsibleControlPlaneReconciler) ensureOpenStackInfraStatusReady(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	ref := cluster.Spec.InfrastructureRef
	if ref.APIGroup != "infrastructure.cluster.x-k8s.io" || ref.Kind != "OpenStackCluster" || ref.Name == "" {
		return true, nil
	}

	var u unstructured.Unstructured
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.APIGroup, Version: "v1beta1", Kind: ref.Kind})
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}, &u); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// 判断是否启用 bastion：spec.bastion.enabled 默认 true；若明确设为 false 则跳过等待
	bastionEnabled := true
	if bm, found, _ := unstructured.NestedMap(u.Object, "spec", "bastion"); found {
		if v, ok := bm["enabled"].(bool); ok {
			bastionEnabled = v
		}
	}
	if !bastionEnabled {
		return true, nil
	}

	// 若启用 bastion，则要求 status.bastion.state == ACTIVE
	state, _, _ := unstructured.NestedString(u.Object, "status", "bastion", "state")
	if !strings.EqualFold(state, "ACTIVE") {
		return false, nil
	}
	return true, nil
}

func (r *AnsibleControlPlaneReconciler) reconcileClusterCertificates(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	controllerRef := metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind))
	certificates := clustersecret.NewCertificatesForInitialControlPlane(nil)
	if err := certificates.LookupOrGenerate(ctx, r.Client, util.ObjectKey(cluster), *controllerRef); err != nil {
		setACPCondition(acp, controlplanev1alpha1.CertificatesAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.CertificatesGenerationFailedReason, err.Error())
		return err
	}
	setACPCondition(acp, controlplanev1alpha1.CertificatesAvailableCondition, metav1.ConditionTrue, "", "")
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
			setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.KubeconfigGenerationFailedReason, err.Error())
			return err
		}
		owner := metav1.NewControllerRef(cluster, clusterv1.GroupVersion.WithKind("Cluster"))
		if err := clusterkubeconfig.CreateSecretWithOwner(ctx, r.Client, util.ObjectKey(cluster), cluster.Spec.ControlPlaneEndpoint.String(), *owner); err != nil {
			setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.KubeconfigGenerationFailedReason, err.Error())
			return err
		}
	} else {
		if err := r.ensureKubeconfigMetadata(ctx, cluster, existing); err != nil {
			setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.KubeconfigGenerationFailedReason, err.Error())
			return err
		}
	}
	setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionTrue, "", "")
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

// remote API probe implemented in controlplane/ansible/internal/workloadprobe

func (r *AnsibleControlPlaneReconciler) syncMachines(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	controlPlaneDesired := int32(1)
	if acp.Spec.Replicas != nil {
		controlPlaneDesired = *acp.Spec.Replicas
	}
	isEtcdRequired := desiredEtcdReplicas(acp) > 0
	if controlPlaneDesired <= 0 && !isEtcdRequired {
		setACPCondition(acp, controlplanev1alpha1.MachinesCreatedCondition, metav1.ConditionTrue, "", "")
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
			setACPCondition(acp, controlplanev1alpha1.MachinesCreatedCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, msg)
			return nil
		}
	}

	if current >= controlPlaneDesired {
		setACPCondition(acp, controlplanev1alpha1.MachinesCreatedCondition, metav1.ConditionTrue, "", "")
		return r.ensureStandaloneEtcdMachines(ctx, acp, cluster)
	}

	for current < controlPlaneDesired {
		reason, err := r.createControlPlaneMachine(ctx, acp, cluster)
		if err != nil {
			if reason == "" {
				reason = controlplanev1alpha1.MachineCreationFailedReason
			}
			setACPCondition(acp, controlplanev1alpha1.MachinesCreatedCondition, metav1.ConditionFalse, reason, err.Error())
			return err
		}
		current++
	}

	setACPCondition(acp, controlplanev1alpha1.MachinesCreatedCondition, metav1.ConditionTrue, "", "")
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

func (r *AnsibleControlPlaneReconciler) cloneInfrastructureMachine(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, name string, labels, annotations map[string]string) (clusterv1.ContractVersionedObjectReference, error) {
	templateContractRef := acp.Spec.MachineTemplate.Spec.InfrastructureRef
	apiVersion, err := contract.GetAPIVersion(ctx, r.Client, templateContractRef.GroupKind())
	if err != nil {
		return clusterv1.ContractVersionedObjectReference{}, err
	}
	templateRef := &corev1.ObjectReference{
		APIVersion: apiVersion,
		Kind:       templateContractRef.Kind,
		Namespace:  acp.Namespace,
		Name:       templateContractRef.Name,
	}
	owner := metav1.NewControllerRef(acp, controlplanev1alpha1.GroupVersion.WithKind(ansibleControlPlaneKind))
	_, ref, err := external.CreateFromTemplate(ctx, &external.CreateFromTemplateInput{
		Client:      r.Client,
		TemplateRef: templateRef,
		Namespace:   acp.Namespace,
		Name:        name,
		ClusterName: cluster.Name,
		OwnerRef:    owner,
		Labels:      labels,
		Annotations: annotations,
	})
	if err != nil {
		return clusterv1.ContractVersionedObjectReference{}, err
	}
	return ref, nil
}

func (r *AnsibleControlPlaneReconciler) createBootstrapConfig(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, machineName string, labels, annotations map[string]string, roleOverride []string) (clusterv1.ContractVersionedObjectReference, error) {
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
		return clusterv1.ContractVersionedObjectReference{}, err
	}

	return clusterv1.ContractVersionedObjectReference{
		APIGroup: bootstrapv1.GroupVersion.Group,
		Kind:     "AnsibleConfig",
		Name:     cfg.Name,
	}, nil
}

func (r *AnsibleControlPlaneReconciler) adoptBootstrapConfig(ctx context.Context, ref clusterv1.ContractVersionedObjectReference, machine *clusterv1.Machine) error {
	if !ref.IsDefined() {
		return nil
	}
	cfg := &bootstrapv1.AnsibleConfig{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}, cfg); err != nil {
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

func setACPCondition(acp *controlplanev1alpha1.AnsibleControlPlane, conditionType clusterv1.ConditionType, status metav1.ConditionStatus, reason, message string) {
	// Ensure Reason is never empty to satisfy CRD validation (minLength>=1).
	if reason == "" {
		reason = defaultReasonFor(conditionType)
	}
	conditions.Set(acp, metav1.Condition{
		Type:    string(conditionType),
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// defaultReasonFor returns a stable, non-empty Reason for a given condition type
// when callers don't specify one explicitly.
func defaultReasonFor(conditionType clusterv1.ConditionType) string {
	switch conditionType {
	case controlplanev1alpha1.CertificatesAvailableCondition:
		return controlplanev1alpha1.CertificatesAvailableReason
	case controlplanev1alpha1.KubeconfigAvailableCondition:
		return controlplanev1alpha1.KubeconfigGeneratedReason
	case controlplanev1alpha1.MachinesCreatedCondition:
		return controlplanev1alpha1.MachinesCreatedReason
	case controlplanev1alpha1.PostBootstrapReadyCondition:
		return controlplanev1alpha1.PostBootstrapCompletedReason
	case clusterv1.ReadyCondition:
		return "Reconciled"
	default:
		return "Reconciled"
	}
}

func (r *AnsibleControlPlaneReconciler) syncAnchorAnnotations(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	machineList := &clusterv1.MachineList{}
	selectors := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: cluster.Name,
		},
	}
	if err := r.Client.List(ctx, machineList, selectors...); err != nil {
		return err
	}

	primaryName := ""
	if acp.Status.ControlPlaneInitialMachine != nil {
		primaryName = acp.Status.ControlPlaneInitialMachine.Name
	}
	etcdNames := sets.New[string]()
	for i := range acp.Status.EtcdInitialMachines {
		if name := acp.Status.EtcdInitialMachines[i].Name; name != "" {
			etcdNames.Insert(name)
		}
	}

	for i := range machineList.Items {
		machine := &machineList.Items[i]
		wantPrimary := primaryName != "" && machine.Name == primaryName
		if err := r.ensureMachineAnnotation(ctx, machine, ansiblecontract.ControlPlaneInitialMachineAnnotation, wantPrimary); err != nil {
			return err
		}
		wantEtcd := etcdNames.Has(machine.Name)
		if err := r.ensureMachineAnnotation(ctx, machine, ansiblecontract.EtcdInitialMachineAnnotation, wantEtcd); err != nil {
			return err
		}
	}
	return nil
}

func (r *AnsibleControlPlaneReconciler) ensureMachineAnnotation(ctx context.Context, machine *clusterv1.Machine, key string, enabled bool) error {
	annotations := machine.GetAnnotations()
	if enabled {
		if annotations != nil && annotations[key] == ansiblecontract.AnnotationTrueValue() {
			return nil
		}
		original := machine.DeepCopy()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[key] = ansiblecontract.AnnotationTrueValue()
		machine.SetAnnotations(annotations)
		return r.Client.Patch(ctx, machine, client.MergeFrom(original))
	}
	if annotations == nil {
		return nil
	}
	if _, ok := annotations[key]; !ok {
		return nil
	}
	original := machine.DeepCopy()
	delete(annotations, key)
	if len(annotations) == 0 {
		machine.SetAnnotations(nil)
	} else {
		machine.SetAnnotations(annotations)
	}
	return r.Client.Patch(ctx, machine, client.MergeFrom(original))
}
