/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	ansiblecloudinit "sigs.k8s.io/cluster-api/bootstrap/ansible/internal/cloudinit"
	"sigs.k8s.io/cluster-api/bootstrap/ansible/internal/locking"
	bsutil "sigs.k8s.io/cluster-api/bootstrap/util"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
	expv1 "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	configMapLockDuration      = 30 * time.Second
	configMapLockRetryInterval = 2 * time.Second
	initLockRetryInterval      = 30 * time.Second

	defaultBootstrapFileOwner = "root:root"
	defaultBootstrapFilePerm  = "0644"

	bootstrapCompleteCommand = "echo \"Ansible pre-bootstrap completed\""
)

// InitLocker coordinates the first control plane initialization.
type InitLocker interface {
	Lock(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) bool
	Unlock(ctx context.Context, cluster *clusterv1.Cluster) bool
}

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=ansibleconfigs;ansibleconfigs/status;ansibleconfigs/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status;machinesets;machines;machines/status;machinepools;machinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;events;configmaps,verbs=get;list;watch;create;update;patch;delete

// AnsibleConfigReconciler reconciles a AnsibleConfig object.
type AnsibleConfigReconciler struct {
	Client              client.Client
	SecretCachingClient client.Client
	InitLock            InitLocker
	ConfigMapLeaseLock  *locking.LeaseLock
	DynamicClient       dynamic.Interface
	RESTMapper          meta.RESTMapper

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// Scope is a scoped struct used during reconciliation.
type Scope struct {
	logr.Logger
	Config                   *bootstrapv1.AnsibleConfig
	ConfigOwner              *bsutil.ConfigOwner
	Cluster                  *clusterv1.Cluster
	heldLocks                map[string]lockReference
	initLockHeld             bool
	ClusterTemplate          *bootstrapv1.ResourceTemplate
	ClusterOperationTemplate *bootstrapv1.ResourceTemplate
	ClusterOperationPlan     clusterOperationPlan
}

type clusterOperationPlan struct {
	ActionType string
	Vars       map[string]interface{}
}

func defaultClusterOperationPlan() clusterOperationPlan {
	return clusterOperationPlan{ActionType: bootstrapv1.ClusterOperationActionCluster}
}

// SetupWithManager sets up the reconciler with the Manager.
func (r *AnsibleConfigReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	if r.ConfigMapLeaseLock == nil {
		r.ConfigMapLeaseLock = locking.NewLeaseLock(mgr.GetClient(), configMapLockDuration)
	}
	if r.DynamicClient == nil {
		dynClient, err := dynamic.NewForConfig(mgr.GetConfig())
		if err != nil {
			return errors.Wrap(err, "failed to build dynamic client")
		}
		r.DynamicClient = dynClient
	}
	if r.RESTMapper == nil {
		r.RESTMapper = mgr.GetRESTMapper()
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&bootstrapv1.AnsibleConfig{}).
		WithOptions(options).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.MachineToBootstrapMapFunc),
		).WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue))

	if feature.Gates.Enabled(feature.MachinePool) {
		b = b.Watches(
			&expv1.MachinePool{},
			handler.EnqueueRequestsFromMapFunc(r.MachinePoolToBootstrapMapFunc),
		)
	}

	b = b.Watches(
		&clusterv1.Cluster{},
		handler.EnqueueRequestsFromMapFunc(r.ClusterToAnsibleConfigs),
		builder.WithPredicates(
			predicates.All(ctrl.LoggerFrom(ctx),
				predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(ctx)),
				predicates.ResourceHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue),
			),
		),
	)

	b = b.Watches(
		&controlplanev1alpha1.AnsibleControlPlane{},
		handler.EnqueueRequestsFromMapFunc(r.ControlPlaneToAnsibleConfigs),
		builder.WithPredicates(
			predicates.All(ctrl.LoggerFrom(ctx),
				predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue),
			),
		),
	)

	if err := b.Complete(r); err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	return nil
}

// Reconcile handles AnsibleConfig events.
func (r *AnsibleConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := ctrl.LoggerFrom(ctx)

	// Look up the ansible config
	config := &bootstrapv1.AnsibleConfig{}
	if err := r.Client.Get(ctx, req.NamespacedName, config); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Look up the owner of this ansible config if there is one
	configOwner, err := bsutil.GetTypedConfigOwner(ctx, r.Client, config)
	if apierrors.IsNotFound(err) {
		// Could not find the owner yet, this is not an error and will rereconcile when the owner gets set.
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get owner")
	}
	if configOwner == nil {
		return ctrl.Result{}, nil
	}
	log = log.WithValues(configOwner.GetKind(), klog.KRef(configOwner.GetNamespace(), configOwner.GetName()), "resourceVersion", configOwner.GetResourceVersion())
	ctx = ctrl.LoggerInto(ctx, log)

	// Lookup the cluster the config owner is associated with
	cluster, err := util.GetClusterByName(ctx, r.Client, configOwner.GetNamespace(), configOwner.ClusterName())
	if err != nil {
		if errors.Cause(err) == util.ErrNoCluster {
			log.Info(fmt.Sprintf("%s does not belong to a cluster yet, waiting until it's part of a cluster", configOwner.GetKind()))
			return ctrl.Result{}, nil
		}

		if apierrors.IsNotFound(err) {
			log.Info("Cluster does not exist yet, waiting until it is created")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Could not get cluster with metadata")
		return ctrl.Result{}, err
	}

	if annotations.IsPaused(cluster, config) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	scope := &Scope{
		Logger:               log,
		Config:               config,
		ConfigOwner:          configOwner,
		Cluster:              cluster,
		heldLocks:            map[string]lockReference{},
		ClusterOperationPlan: defaultClusterOperationPlan(),
	}

	defer func() {
		if scope.initLockHeld && rerr != nil && r.InitLock != nil {
			if !r.InitLock.Unlock(ctx, scope.Cluster) {
				scope.Logger.Info("failed to release init lock after reconciliation error")
				return
			}
			scope.initLockHeld = false
		}
	}()

	clusterTemplate, err := buildClusterResourceTemplate(scope)
	if err != nil {
		return ctrl.Result{}, err
	}
	scope.ClusterTemplate = &clusterTemplate

	plan, requeueAfter, err := r.determineClusterOperationPlan(ctx, scope)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueAfter != nil {
		return *requeueAfter, nil
	}
	scope.ClusterOperationPlan = plan

	clusterOpsTemplate, err := buildClusterOperationResourceTemplate(scope, clusterTemplate)
	if err != nil {
		return ctrl.Result{}, err
	}
	scope.ClusterOperationTemplate = &clusterOpsTemplate

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(config, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Attempt to Patch the AnsibleConfig object and status after each reconciliation if no error occurs.
	defer func() {
		// always update the readyCondition; the summary is represented using the "1 of x completed" notation.
		conditions.SetSummary(config,
			conditions.WithConditions(
				bootstrapv1.DataSecretAvailableCondition,
			),
		)
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{}
		if rerr == nil {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		if err := patchHelper.Patch(ctx, config, patchOpts...); err != nil {
			rerr = kerrors.NewAggregate([]error{rerr, err})
		}
	}()

	// Ignore deleted AnsibleConfigs.
	if !config.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	return r.reconcile(ctx, scope, cluster, config, configOwner)
}

func (r *AnsibleConfigReconciler) reconcile(ctx context.Context, scope *Scope, cluster *clusterv1.Cluster, config *bootstrapv1.AnsibleConfig, configOwner *bsutil.ConfigOwner) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Ensure the bootstrap secret associated with this AnsibleConfig has the correct ownerReference.
	if err := r.ensureBootstrapSecretOwnersRef(ctx, scope); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile status for machines that already have a secret reference, but our status isn't up to date.
	// This case solves the pivoting scenario (or a backup restore) which doesn't preserve the status subresource on objects.
	if configOwner.DataSecretName() != nil && (!config.Status.Ready || config.Status.DataSecretName == nil) {
		config.Status.Ready = true
		config.Status.DataSecretName = configOwner.DataSecretName()
		markInitializationDataSecretCreated(config)
		conditions.MarkTrue(config, bootstrapv1.DataSecretAvailableCondition)
		return ctrl.Result{}, nil
	}

	if !config.Status.Ready {
		if err := r.reconcilePreBootstrap(ctx, scope); err != nil {
			log.Error(err, "Failed to generate bootstrap data")
			conditions.MarkFalse(config, bootstrapv1.DataSecretAvailableCondition, bootstrapv1.DataSecretGenerationFailedReason, clusterv1.ConditionSeverityError, err.Error())
			return ctrl.Result{}, err
		}
	}

	result, err := r.reconcilePostBootstrap(ctx, scope)
	if err != nil {
		return result, err
	}
	return result, nil
}

// storeBootstrapData creates a new secret with the data passed in as input,
// sets the reference in the configuration status and ready to true.
func (r *AnsibleConfigReconciler) storeBootstrapData(ctx context.Context, scope *Scope, data []byte) error {
	log := ctrl.LoggerFrom(ctx)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scope.Config.Name,
			Namespace: scope.Config.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: scope.Cluster.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: bootstrapv1.GroupVersion.String(),
					Kind:       "AnsibleConfig",
					Name:       scope.Config.Name,
					UID:        scope.Config.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Data: map[string][]byte{
			"value": data,
		},
		Type: clusterv1.ClusterSecretType,
	}

	// as secret creation and scope.Config status patch are not atomic operations
	// it is possible that secret creation happens but the config.Status patches are not applied
	if err := r.Client.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrapf(err, "failed to create bootstrap data secret for AnsibleConfig %s/%s", scope.Config.Namespace, scope.Config.Name)
		}
		log.Info("Bootstrap data secret for AnsibleConfig already exists, updating", "Secret", klog.KObj(secret))
		if err := r.Client.Update(ctx, secret); err != nil {
			return errors.Wrapf(err, "failed to update bootstrap data secret for AnsibleConfig %s/%s", scope.Config.Namespace, scope.Config.Name)
		}
	}
	scope.Config.Status.DataSecretName = ptr.To(secret.Name)
	scope.Config.Status.Ready = true
	markInitializationDataSecretCreated(scope.Config)
	conditions.MarkTrue(scope.Config, bootstrapv1.DataSecretAvailableCondition)
	return nil
}

func (r *AnsibleConfigReconciler) reconcilePreBootstrap(ctx context.Context, scope *Scope) error {
	script, err := r.renderBootstrapData(ctx, scope)
	if err != nil {
		return err
	}
	return r.storeBootstrapData(ctx, scope, script)
}

func (r *AnsibleConfigReconciler) renderBootstrapData(ctx context.Context, scope *Scope) ([]byte, error) {
	certSecret, secretName, err := r.fetchCertificateSecret(ctx, scope)
	if err != nil {
		return nil, err
	}
	ca, ok := certSecret.Data["tls.crt"]
	if !ok {
		return nil, errors.Errorf("certificate secret %s/%s is missing tls.crt", secretName.Namespace, secretName.Name)
	}
	key, ok := certSecret.Data["tls.key"]
	if !ok {
		return nil, errors.Errorf("certificate secret %s/%s is missing tls.key", secretName.Namespace, secretName.Name)
	}
	files := buildBootstrapFiles(ca, key, scope.Config.Spec.Files)
	userData := &ansiblecloudinit.BaseUserData{
		WriteFiles:  files,
		RunCommands: []string{bootstrapCompleteCommand},
	}
	rendered, err := ansiblecloudinit.Render(userData)
	if err != nil {
		return nil, errors.Wrap(err, "failed to render cloud-init for AnsibleConfig")
	}
	return rendered, nil
}

func (r *AnsibleConfigReconciler) fetchCertificateSecret(ctx context.Context, scope *Scope) (*corev1.Secret, types.NamespacedName, error) {
	secretName := certificateSecretKey(scope)
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, secretName, errors.Wrapf(err, "certificate secret %s/%s not found", secretName.Namespace, secretName.Name)
		}
		return nil, secretName, errors.Wrapf(err, "failed to fetch certificate secret %s/%s", secretName.Namespace, secretName.Name)
	}
	return secret, secretName, nil
}

func certificateSecretKey(scope *Scope) types.NamespacedName {
	name := scope.Config.Spec.CertRef.Name
	namespace := scope.Config.Spec.CertRef.Namespace
	if namespace == "" {
		namespace = scope.Config.Namespace
	}
	if name == "" {
		name = fmt.Sprintf("%s-ca", scope.Cluster.Name)
	}
	return types.NamespacedName{Namespace: namespace, Name: name}
}

func (r *AnsibleConfigReconciler) reconcilePostBootstrap(ctx context.Context, scope *Scope) (ctrl.Result, error) {
	if scope.Config.Status.PostBootstrapCompleted {
		return ctrl.Result{}, nil
	}

	if !scope.ConfigOwner.IsInfrastructureReady() {
		scope.Logger.Info("Waiting for machine infrastructure to report ready before launching Ansible operations")
		return ctrl.Result{}, nil
	}

	clusterDepsReady, err := r.ensureKubeanClusterDependencies(ctx, scope, *scope.ClusterTemplate)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !clusterDepsReady {
		return ctrl.Result{RequeueAfter: configMapLockRetryInterval}, nil
	}

	if err := r.applyResourceFromTemplate(ctx, scope, *scope.ClusterTemplate); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyResourceFromTemplate(ctx, scope, *scope.ClusterOperationTemplate); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.releaseHeldLocks(ctx, scope); err != nil {
		return ctrl.Result{}, err
	}

	scope.Config.Status.PostBootstrapCompleted = true
	return ctrl.Result{}, nil
}

func (r *AnsibleConfigReconciler) applyResourceFromTemplate(ctx context.Context, scope *Scope, template bootstrapv1.ResourceTemplate) error {
	rendered, err := buildResourceFromTemplate(scope, template)
	if err != nil {
		return err
	}

	current := &unstructured.Unstructured{}
	current.SetAPIVersion(rendered.GetAPIVersion())
	current.SetKind(rendered.GetKind())
	err = r.Client.Get(ctx, client.ObjectKey{Namespace: rendered.GetNamespace(), Name: rendered.GetName()}, current)
	if apierrors.IsNotFound(err) {
		scope.Logger.Info("Creating ansible dependency resource", "gvk", rendered.GroupVersionKind().String(), "name", rendered.GetName(), "namespace", rendered.GetNamespace())
		return r.Client.Create(ctx, rendered)
	}
	if err != nil {
		return errors.Wrapf(err, "failed to get %s %s/%s", rendered.GroupVersionKind().String(), rendered.GetNamespace(), rendered.GetName())
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.SetLabels(rendered.GetLabels())
	current.SetAnnotations(rendered.GetAnnotations())
	current.SetOwnerReferences(rendered.GetOwnerReferences())
	if spec, ok := rendered.Object["spec"]; ok {
		current.Object["spec"] = spec
	} else {
		delete(current.Object, "spec")
	}

	if err := r.Client.Patch(ctx, current, patch); err != nil {
		return errors.Wrapf(err, "failed to patch %s %s/%s", rendered.GroupVersionKind().String(), rendered.GetNamespace(), rendered.GetName())
	}
	return nil
}

func buildResourceFromTemplate(scope *Scope, template bootstrapv1.ResourceTemplate) (*unstructured.Unstructured, error) {
	if template.APIVersion == "" || template.Kind == "" || template.Name == "" {
		return nil, errors.New("resource template requires apiVersion, kind, and name")
	}

	ns := template.Namespace
	if ns == "" {
		ns = scope.Config.Namespace
	}

	rendered := &unstructured.Unstructured{}
	rendered.SetAPIVersion(template.APIVersion)
	rendered.SetKind(template.Kind)
	rendered.SetNamespace(ns)
	rendered.SetName(template.Name)

	labels := map[string]string{}
	for k, v := range template.Labels {
		labels[k] = v
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels[clusterv1.ClusterNameLabel]; !ok {
		labels[clusterv1.ClusterNameLabel] = scope.Cluster.Name
	}
	rendered.SetLabels(labels)

	if len(template.Annotations) > 0 {
		annotations := map[string]string{}
		for k, v := range template.Annotations {
			annotations[k] = v
		}
		rendered.SetAnnotations(annotations)
	}

	rendered.SetOwnerReferences(util.EnsureOwnerRef(nil, metav1.OwnerReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "AnsibleConfig",
		Name:       scope.Config.Name,
		UID:        scope.Config.UID,
		Controller: ptr.To(true),
	}))

	if len(template.Spec.Raw) > 0 {
		spec := map[string]interface{}{}
		if err := json.Unmarshal(template.Spec.Raw, &spec); err != nil {
			return nil, errors.Wrap(err, "failed to decode resource template spec")
		}
		if err := unstructured.SetNestedField(rendered.Object, spec, "spec"); err != nil {
			return nil, errors.Wrap(err, "failed to assign spec to rendered resource")
		}
	}

	return rendered, nil
}

func buildBootstrapFiles(ca, key []byte, extraFiles []bootstrapv1.File) []ansiblecloudinit.File {
	files := []ansiblecloudinit.File{
		{
			Path:        "/etc/kubernetes/ssl/ca.pem",
			Owner:       defaultBootstrapFileOwner,
			Permissions: defaultBootstrapFilePerm,
			Content:     string(ca),
		},
		{
			Path:        "/etc/kubernetes/ssl/ca-key.pem",
			Owner:       defaultBootstrapFileOwner,
			Permissions: "0600",
			Content:     string(key),
		},
	}

	for _, f := range extraFiles {
		if f.Path == "" {
			continue
		}
		owner := f.Owner
		if owner == "" {
			owner = defaultBootstrapFileOwner
		}
		perms := f.Permissions
		if perms == "" {
			perms = defaultBootstrapFilePerm
		}
		files = append(files, ansiblecloudinit.File{
			Path:        f.Path,
			Owner:       owner,
			Permissions: perms,
			Content:     f.Content,
		})
	}
	return files
}

// Ensure the bootstrap secret has the AnsibleConfig as a controller OwnerReference.
func (r *AnsibleConfigReconciler) ensureBootstrapSecretOwnersRef(ctx context.Context, scope *Scope) error {
	secret := &corev1.Secret{}
	err := r.SecretCachingClient.Get(ctx, client.ObjectKey{Namespace: scope.Config.Namespace, Name: scope.Config.Name}, secret)
	if err != nil {
		// If the secret has not been created yet return early.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "failed to add AnsibleConfig %s as ownerReference to bootstrap Secret %s", scope.ConfigOwner.GetName(), secret.GetName())
	}
	shouldUpdateData := len(secret.Data["value"]) == 0
	var rendered []byte
	if shouldUpdateData {
		var renderErr error
		rendered, renderErr = r.renderBootstrapData(ctx, scope)
		if renderErr != nil {
			return renderErr
		}
	}
	patchHelper, err := patch.NewHelper(secret, r.Client)
	if err != nil {
		return errors.Wrapf(err, "failed to add AnsibleConfig %s as ownerReference to bootstrap Secret %s", scope.ConfigOwner.GetName(), secret.GetName())
	}
	if c := metav1.GetControllerOf(secret); c != nil && c.Kind != "AnsibleConfig" {
		secret.SetOwnerReferences(util.RemoveOwnerRef(secret.GetOwnerReferences(), *c))
	}
	secret.SetOwnerReferences(util.EnsureOwnerRef(secret.GetOwnerReferences(), metav1.OwnerReference{
		APIVersion: bootstrapv1.GroupVersion.String(),
		Kind:       "AnsibleConfig",
		UID:        scope.Config.UID,
		Name:       scope.Config.Name,
		Controller: ptr.To(true),
	}))
	if shouldUpdateData {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data["value"] = rendered
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[clusterv1.ClusterNameLabel] = scope.Cluster.Name
		secret.Type = clusterv1.ClusterSecretType
	}
	err = patchHelper.Patch(ctx, secret)
	if err != nil {
		return errors.Wrapf(err, "could not add AnsibleConfig %s as ownerReference to bootstrap Secret %s", scope.ConfigOwner.GetName(), secret.GetName())
	}
	return nil
}

// ClusterToAnsibleConfigs is a handler.ToRequestsFunc to be used to enqueue
// requests for reconciliation of AnsibleConfigs.
func (r *AnsibleConfigReconciler) ClusterToAnsibleConfigs(ctx context.Context, o client.Object) []ctrl.Request {
	result := []ctrl.Request{}

	c, ok := o.(*clusterv1.Cluster)
	if !ok {
		panic(fmt.Sprintf("Expected a Cluster but got a %T", o))
	}

	selectors := []client.ListOption{
		client.InNamespace(c.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: c.Name,
		},
	}

	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machineList, selectors...); err != nil {
		return nil
	}

	for _, m := range machineList.Items {
		if m.Spec.Bootstrap.ConfigRef != nil &&
			m.Spec.Bootstrap.ConfigRef.GroupVersionKind().GroupKind() == bootstrapv1.GroupVersion.WithKind("AnsibleConfig").GroupKind() {
			name := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.Bootstrap.ConfigRef.Name}
			result = append(result, ctrl.Request{NamespacedName: name})
		}
	}

	if feature.Gates.Enabled(feature.MachinePool) {
		machinePoolList := &expv1.MachinePoolList{}
		if err := r.Client.List(ctx, machinePoolList, selectors...); err != nil {
			return nil
		}

		for _, mp := range machinePoolList.Items {
			if mp.Spec.Template.Spec.Bootstrap.ConfigRef != nil &&
				mp.Spec.Template.Spec.Bootstrap.ConfigRef.GroupVersionKind().GroupKind() == bootstrapv1.GroupVersion.WithKind("AnsibleConfig").GroupKind() {
				name := client.ObjectKey{Namespace: mp.Namespace, Name: mp.Spec.Template.Spec.Bootstrap.ConfigRef.Name}
				result = append(result, ctrl.Request{NamespacedName: name})
			}
		}
	}

	return result
}

// ControlPlaneToAnsibleConfigs enqueues AnsibleConfigs whenever the owning ACP status changes.
func (r *AnsibleConfigReconciler) ControlPlaneToAnsibleConfigs(ctx context.Context, o client.Object) []ctrl.Request {
	acp, ok := o.(*controlplanev1alpha1.AnsibleControlPlane)
	if !ok {
		return nil
	}
	clusterName := acp.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		return nil
	}
	machineList := &clusterv1.MachineList{}
	selectors := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel:         clusterName,
			clusterv1.MachineControlPlaneLabel: "",
		},
	}
	if err := r.Client.List(ctx, machineList, selectors...); err != nil {
		return nil
	}
	result := []ctrl.Request{}
	for i := range machineList.Items {
		m := machineList.Items[i]
		if m.Spec.Bootstrap.ConfigRef != nil &&
			m.Spec.Bootstrap.ConfigRef.GroupVersionKind().GroupKind() == bootstrapv1.GroupVersion.WithKind("AnsibleConfig").GroupKind() {
			name := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.Bootstrap.ConfigRef.Name}
			result = append(result, ctrl.Request{NamespacedName: name})
		}
	}
	return result
}

func (r *AnsibleConfigReconciler) ensureKubeanClusterDependencies(ctx context.Context, scope *Scope, template bootstrapv1.ResourceTemplate) (bool, error) {
	specMap, err := decodeTemplateSpec(template.Spec)
	if err != nil {
		return false, err
	}
	defaultNamespace := template.Namespace
	if defaultNamespace == "" {
		defaultNamespace = scope.Config.Namespace
	}
	tasks := []configMapTask{}
	if ref := parseNamespacedConfigMapRef(specMap, "hostsConfRef", defaultNamespace); ref.Name != "" {
		tasks = append(tasks, configMapTask{
			reference: ref,
			builder: func() (map[string]string, error) {
				inventory, invErr := r.buildHostsInventory(ctx, scope)
				if invErr != nil {
					return nil, invErr
				}
				return map[string]string{"inventory.ini": inventory}, nil
			},
		})
	}
	if ref := parseNamespacedConfigMapRef(specMap, "varsConfRef", defaultNamespace); ref.Name != "" {
		tasks = append(tasks, configMapTask{
			reference: ref,
			builder: func() (map[string]string, error) {
				rendered, err := r.renderVarsConfig(ctx, scope)
				if err != nil {
					return nil, err
				}
				return map[string]string{"vars.yaml": rendered}, nil
			},
		})
	}
	if len(tasks) > 0 {
		ready, err := r.ensureConfigMapsWithLock(ctx, scope, tasks)
		if err != nil || !ready {
			return ready, err
		}
	}
	if ref := parseNamespacedSecretRef(specMap, "sshAuthRef", defaultNamespace); ref.Name != "" {
		ready, err := r.ensureSSHAuthSecret(ctx, scope, ref)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}

func (r *AnsibleConfigReconciler) withLeaseLock(ctx context.Context, scope *Scope, ref configMapReference, retain bool, fn func() error) (bool, error) {
	if r.ConfigMapLeaseLock == nil {
		r.ConfigMapLeaseLock = locking.NewLeaseLock(r.Client, configMapLockDuration)
	}
	lockNamespace := ref.Namespace
	if lockNamespace == "" {
		lockNamespace = scope.Config.Namespace
	}
	lockName := locking.GenerateLeaseName(ref.Name)
	holder := fmt.Sprintf("%s/%s", scope.Config.Namespace, scope.Config.Name)
	meta := locking.LeaseMetadata{
		Namespace: lockNamespace,
		Name:      lockName,
		Holder:    holder,
		Labels: map[string]string{
			clusterv1.ClusterNameLabel: scope.Cluster.Name,
		},
		Logger: scope.Logger,
	}
	if lockNamespace == scope.Config.Namespace {
		meta.OwnerReferences = util.EnsureOwnerRef(nil, metav1.OwnerReference{
			APIVersion: bootstrapv1.GroupVersion.String(),
			Kind:       "AnsibleConfig",
			Name:       scope.Config.Name,
			UID:        scope.Config.UID,
			Controller: ptr.To(true),
		})
	}
	acquired, err := r.ConfigMapLeaseLock.WithLease(ctx, meta, retain, fn)
	if err != nil || !acquired {
		return acquired, err
	}
	if retain {
		scope.rememberLock(lockNamespace, lockName)
	}
	return true, nil
}

func (r *AnsibleConfigReconciler) releaseHeldLocks(ctx context.Context, scope *Scope) error {
	if len(scope.heldLocks) == 0 {
		return nil
	}
	if r.ConfigMapLeaseLock == nil {
		return nil
	}
	for _, lock := range scope.heldLockList() {
		if err := r.ConfigMapLeaseLock.Delete(ctx, lock.Namespace, lock.Name); err != nil {
			return err
		}
	}
	scope.resetLocks()
	return nil
}

type configMapReference struct {
	Name      string
	Namespace string
}

type lockReference struct {
	Namespace string
	Name      string
}

type configMapTask struct {
	reference configMapReference
	builder   configMapDataBuilder
}

type configMapDataBuilder func() (map[string]string, error)

type secretReference struct {
	Name      string
	Namespace string
}

func decodeTemplateSpec(raw runtime.RawExtension) (map[string]interface{}, error) {
	if len(raw.Raw) == 0 {
		return map[string]interface{}{}, nil
	}
	spec := map[string]interface{}{}
	if err := json.Unmarshal(raw.Raw, &spec); err != nil {
		return nil, errors.Wrap(err, "failed to decode resource template spec")
	}
	return spec, nil
}

func parseNamespacedConfigMapRef(spec map[string]interface{}, field, defaultNamespace string) configMapReference {
	raw, ok := spec[field]
	if !ok {
		return configMapReference{}
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return configMapReference{}
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return configMapReference{}
	}
	namespace, _ := obj["namespace"].(string)
	if namespace == "" {
		namespace = defaultNamespace
	}
	return configMapReference{Name: name, Namespace: namespace}
}

func (s *Scope) rememberLock(namespace, name string) {
	if name == "" {
		return
	}
	if namespace == "" {
		return
	}
	if s.heldLocks == nil {
		s.heldLocks = map[string]lockReference{}
	}
	key := fmt.Sprintf("%s/%s", namespace, name)
	s.heldLocks[key] = lockReference{
		Namespace: namespace,
		Name:      name,
	}
}

func (s *Scope) heldLockList() []lockReference {
	if len(s.heldLocks) == 0 {
		return nil
	}
	locks := make([]lockReference, 0, len(s.heldLocks))
	for _, l := range s.heldLocks {
		locks = append(locks, l)
	}
	return locks
}

func (s *Scope) resetLocks() {
	s.heldLocks = map[string]lockReference{}
}

func parseNamespacedSecretRef(spec map[string]interface{}, field, defaultNamespace string) secretReference {
	raw, ok := spec[field]
	if !ok {
		return secretReference{}
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		return secretReference{}
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return secretReference{}
	}
	namespace, _ := obj["namespace"].(string)
	if namespace == "" {
		namespace = defaultNamespace
	}
	return secretReference{Name: name, Namespace: namespace}
}

func (r *AnsibleConfigReconciler) ensureSSHAuthSecret(ctx context.Context, scope *Scope, ref secretReference) (bool, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = scope.Config.Namespace
	}
	lockRef := configMapReference{Name: ref.Name, Namespace: ns}
	acquired, err := r.withLeaseLock(ctx, scope, lockRef, true, func() error {
		key := types.NamespacedName{Namespace: ns, Name: ref.Name}
		secret := &corev1.Secret{}
		if err := r.Client.Get(ctx, key, secret); err != nil {
			if !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to get Secret %s/%s", ns, ref.Name)
			}
			newSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ref.Name,
					Namespace: ns,
					Labels: map[string]string{
						clusterv1.ClusterNameLabel: scope.Cluster.Name,
					},
				},
				Type: corev1.SecretTypeOpaque,
			}
			if ns == scope.Config.Namespace {
				newSecret.SetOwnerReferences(util.EnsureOwnerRef(nil, metav1.OwnerReference{
					APIVersion: bootstrapv1.GroupVersion.String(),
					Kind:       "AnsibleConfig",
					Name:       scope.Config.Name,
					UID:        scope.Config.UID,
					Controller: ptr.To(true),
				}))
			}
			if err := r.Client.Create(ctx, newSecret); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return nil
				}
				return errors.Wrapf(err, "failed to create Secret %s/%s", ns, ref.Name)
			}
			return nil
		}

		updated := false
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		if secret.Labels[clusterv1.ClusterNameLabel] != scope.Cluster.Name {
			secret.Labels[clusterv1.ClusterNameLabel] = scope.Cluster.Name
			updated = true
		}
		if ns == scope.Config.Namespace {
			ownerRef := metav1.OwnerReference{
				APIVersion: bootstrapv1.GroupVersion.String(),
				Kind:       "AnsibleConfig",
				Name:       scope.Config.Name,
				UID:        scope.Config.UID,
				Controller: ptr.To(true),
			}
			ownerRefs := util.EnsureOwnerRef(secret.GetOwnerReferences(), ownerRef)
			if !reflect.DeepEqual(ownerRefs, secret.GetOwnerReferences()) {
				secret.SetOwnerReferences(ownerRefs)
				updated = true
			}
		}
		if !updated {
			return nil
		}
		if err := r.Client.Update(ctx, secret); err != nil {
			return errors.Wrapf(err, "failed to update Secret %s/%s", ns, ref.Name)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	return true, nil
}

func markInitializationDataSecretCreated(config *bootstrapv1.AnsibleConfig) {
	if config.Status.Initialization == nil {
		config.Status.Initialization = &bootstrapv1.BootstrapDataInitializationStatus{}
	}
	config.Status.Initialization.DataSecretCreated = true
}

// MachineToBootstrapMapFunc is a handler.ToRequestsFunc to be used to enqueue
// request for reconciliation of AnsibleConfig.
func (r *AnsibleConfigReconciler) MachineToBootstrapMapFunc(_ context.Context, o client.Object) []ctrl.Request {
	m, ok := o.(*clusterv1.Machine)
	if !ok {
		panic(fmt.Sprintf("Expected a Machine but got a %T", o))
	}

	result := []ctrl.Request{}
	if m.Spec.Bootstrap.ConfigRef != nil && m.Spec.Bootstrap.ConfigRef.GroupVersionKind() == bootstrapv1.GroupVersion.WithKind("AnsibleConfig") {
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.Bootstrap.ConfigRef.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}
	return result
}

// MachinePoolToBootstrapMapFunc is a handler.ToRequestsFunc to be used to enqueue
// request for reconciliation of AnsibleConfig.
func (r *AnsibleConfigReconciler) MachinePoolToBootstrapMapFunc(_ context.Context, o client.Object) []ctrl.Request {
	m, ok := o.(*expv1.MachinePool)
	if !ok {
		panic(fmt.Sprintf("Expected a MachinePool but got a %T", o))
	}

	result := []ctrl.Request{}
	configRef := m.Spec.Template.Spec.Bootstrap.ConfigRef
	if configRef != nil && configRef.GroupVersionKind().GroupKind() == bootstrapv1.GroupVersion.WithKind("AnsibleConfig").GroupKind() {
		name := client.ObjectKey{Namespace: m.Namespace, Name: configRef.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}
	return result
}
