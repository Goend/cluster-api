package controllers

import (
	"context"
	"reflect"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
					// 用标签追踪 AC 归属，避免与外部控制器 OwnerRef 冲突
					"bootstrap.cluster.x-k8s.io/ac-ns":   scope.Config.Namespace,
					"bootstrap.cluster.x-k8s.io/ac-name": scope.Config.Name,
				},
			},
			Data: desiredData,
		}
		// 不再写 AC OwnerReference，统一通过标签进行清理
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
	// 确保追踪标签存在
	if cm.Labels["bootstrap.cluster.x-k8s.io/ac-ns"] != scope.Config.Namespace {
		cm.Labels["bootstrap.cluster.x-k8s.io/ac-ns"] = scope.Config.Namespace
		updated = true
	}
	if cm.Labels["bootstrap.cluster.x-k8s.io/ac-name"] != scope.Config.Name {
		cm.Labels["bootstrap.cluster.x-k8s.io/ac-name"] = scope.Config.Name
		updated = true
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
