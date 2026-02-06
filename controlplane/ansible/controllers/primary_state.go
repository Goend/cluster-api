package controllers

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/conditions"
)

func (r *AnsibleControlPlaneReconciler) handlePrimaryState(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, primary *clusterv1.Machine, postBootstrapReady, nodeReady bool) error {
	switch {
	case primary == nil:
		conditions.MarkFalse(acp, controlplanev1alpha1.PostBootstrapReadyCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, "waiting for the first control plane Machine to be created")
		conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, "waiting for the first control plane Machine to be created")
		return r.releaseInitLease(ctx, acp)
	case !postBootstrapReady:
		if err := r.holdInitLease(ctx, acp, primary.Name); err != nil {
			return err
		}
		msg := "waiting for Ansible post bootstrap to finish on the first control plane Machine"
		conditions.MarkFalse(acp, controlplanev1alpha1.PostBootstrapReadyCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, msg)
		conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, msg)
		return nil
	case postBootstrapReady && !nodeReady:
		if err := r.holdInitLease(ctx, acp, primary.Name); err != nil {
			return err
		}
		msg := "waiting for the first control plane Machine to register a Kubernetes Node"
		conditions.MarkTrue(acp, controlplanev1alpha1.PostBootstrapReadyCondition)
		conditions.MarkFalse(acp, controlplanev1alpha1.KubeconfigAvailableCondition, controlplanev1alpha1.WaitingForPrimaryMachineReason, clusterv1.ConditionSeverityInfo, msg)
		return nil
	default:
		conditions.MarkTrue(acp, controlplanev1alpha1.PostBootstrapReadyCondition)
		if err := r.releaseInitLease(ctx, acp); err != nil {
			return err
		}
		return r.reconcileKubeconfig(ctx, acp, cluster)
	}
}
