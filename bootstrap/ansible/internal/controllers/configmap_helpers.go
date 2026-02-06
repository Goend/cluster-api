package controllers

import (
	"context"
	"reflect"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util"
)

func (r *AnsibleConfigReconciler) ensureConfigMapsWithLock(ctx context.Context, scope *Scope, tasks []configMapTask) (bool, error) {
	for _, task := range tasks {
		t := task
		acquired, err := r.withLeaseLock(ctx, scope, t.reference, true, func() error {
			return r.ensureConfigMapExists(ctx, scope, t.reference, t.builder)
		})
		if err != nil {
			return false, err
		}
		if !acquired {
			return false, nil
		}
	}
	return true, nil
}

func (r *AnsibleConfigReconciler) ensureConfigMapExists(ctx context.Context, scope *Scope, ref configMapReference, builder configMapDataBuilder) error {
	ns := ref.Namespace
	if ns == "" {
		ns = scope.Config.Namespace
	}
	key := types.NamespacedName{Namespace: ns, Name: ref.Name}
	cm := &corev1.ConfigMap{}
	var desiredData map[string]string
	var err error
	if builder != nil {
		desiredData, err = builder()
		if err != nil {
			return err
		}
	}
	if desiredData == nil {
		desiredData = map[string]string{}
	}
	if err := r.Client.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "failed to get ConfigMap %s/%s", ns, ref.Name)
		}
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ref.Name,
				Namespace: ns,
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: scope.Cluster.Name,
				},
			},
			Data: desiredData,
		}
		if ns == scope.Config.Namespace {
			newCM.SetOwnerReferences(util.EnsureOwnerRef(nil, metav1.OwnerReference{
				APIVersion: bootstrapv1.GroupVersion.String(),
				Kind:       "AnsibleConfig",
				Name:       scope.Config.Name,
				UID:        scope.Config.UID,
				Controller: ptr.To(true),
			}))
		}
		if err := r.Client.Create(ctx, newCM); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return errors.Wrapf(err, "failed to create ConfigMap %s/%s", ns, ref.Name)
		}
		return nil
	}
	if builder == nil {
		return nil
	}
	updated := false
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	if cm.Labels[clusterv1.ClusterNameLabel] != scope.Cluster.Name {
		cm.Labels[clusterv1.ClusterNameLabel] = scope.Cluster.Name
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
		ownerRefs := util.EnsureOwnerRef(cm.GetOwnerReferences(), ownerRef)
		if !reflect.DeepEqual(ownerRefs, cm.GetOwnerReferences()) {
			cm.SetOwnerReferences(ownerRefs)
			updated = true
		}
	}
	if !reflect.DeepEqual(cm.Data, desiredData) {
		cm.Data = desiredData
		updated = true
	}
	if !updated {
		return nil
	}
	if err := r.Client.Update(ctx, cm); err != nil {
		return errors.Wrapf(err, "failed to update ConfigMap %s/%s", ns, ref.Name)
	}
	return nil
}
