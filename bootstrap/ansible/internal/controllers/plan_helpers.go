package controllers

import (
	"context"
	"errors"
	"strconv"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *AnsibleConfigReconciler) determineClusterOperationPlan(ctx context.Context, scope *Scope) (clusterOperationPlan, *ctrl.Result, error) {
	plan := scope.ClusterOperationPlan
	annotations := scope.Config.GetAnnotations()
	action := annotations[bootstrapv1.ClusterOperationActionAnnotation]
	roles := scope.Config.Spec.Role
	workerOnly := isWorkerOnlyRole(roles)
	masterRole := hasMasterRole(roles)

	if action != "" {
		plan.ActionType = action
		if action == bootstrapv1.ClusterOperationActionScale {
			plan.Vars = map[string]interface{}{
				"scale_master": parseScaleMasterAnnotation(annotations),
			}
		} else {
			plan.Vars = nil
		}
		return plan, nil, nil
	}

	clusterInitialized := conditions.IsTrue(scope.Cluster, clusterv1.ControlPlaneInitializedCondition)
	if !clusterInitialized {
		if masterRole {
			requeue, err := r.ensureInitLockHolder(ctx, scope)
			if err != nil {
				return plan, nil, err
			}
			if requeue != nil {
				return plan, requeue, nil
			}
			plan.ActionType = bootstrapv1.ClusterOperationActionCluster
			plan.Vars = nil
			return plan, nil, nil
		}
		scope.Logger.Info("waiting for the first control plane Machine to finish initialization before executing scale action")
		return plan, &ctrl.Result{RequeueAfter: initLockRetryInterval}, nil
	}

	r.releaseInitLock(ctx, scope)
	plan.ActionType = bootstrapv1.ClusterOperationActionScale
	plan.Vars = map[string]interface{}{"scale_master": masterRole && !workerOnly}
	return plan, nil, nil
}

func parseScaleMasterAnnotation(annotations map[string]string) bool {
	raw := annotations[bootstrapv1.ClusterOperationScaleMasterAnnotation]
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return value
}

func (r *AnsibleConfigReconciler) ensureInitLockHolder(ctx context.Context, scope *Scope) (*ctrl.Result, error) {
	if r.InitLock == nil {
		return nil, errors.New("init lock is not configured")
	}
	machine, err := machineFromScope(scope)
	if err != nil {
		return nil, err
	}
	if machine == nil {
		return nil, errors.New("machine owner is required to acquire init lock")
	}
	if !r.InitLock.Lock(ctx, scope.Cluster, machine) {
		scope.Logger.Info("another control plane Machine is initializing, waiting for init lock", "machine", machine.Name)
		return &ctrl.Result{RequeueAfter: initLockRetryInterval}, nil
	}
	scope.initLockHeld = true
	return nil, nil
}

func (r *AnsibleConfigReconciler) releaseInitLock(ctx context.Context, scope *Scope) {
	if r.InitLock == nil || scope.Cluster == nil {
		return
	}
	if r.InitLock.Unlock(ctx, scope.Cluster) {
		scope.initLockHeld = false
	}
}
