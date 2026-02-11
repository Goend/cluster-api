package controllers

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
)

func (r *AnsibleControlPlaneReconciler) handlePrimaryState(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, primary *clusterv1.Machine, postBootstrapReady, nodeReady bool) error {
	switch {
	case primary == nil:
		setACPCondition(acp, controlplanev1alpha1.PostBootstrapReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, "waiting for the first control plane Machine to be created")
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, "waiting for the first control plane Machine to be created")
		return r.releaseInitLease(ctx, acp)
	case !postBootstrapReady:
		if err := r.holdInitLease(ctx, acp, primary.Name); err != nil {
			return err
		}
		msg := "waiting for Ansible post bootstrap to finish on the first control plane Machine"
		setACPCondition(acp, controlplanev1alpha1.PostBootstrapReadyCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, msg)
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, msg)
		return nil
	case postBootstrapReady && !nodeReady:
		if err := r.holdInitLease(ctx, acp, primary.Name); err != nil {
			return err
		}
		msg := "waiting for the first control plane Machine to register a Kubernetes Node"
		setACPCondition(acp, controlplanev1alpha1.PostBootstrapReadyCondition, metav1.ConditionTrue, "", "")
		setACPCondition(acp, controlplanev1alpha1.KubeconfigAvailableCondition, metav1.ConditionFalse, controlplanev1alpha1.WaitingForPrimaryMachineReason, msg)
		return nil
	default:
		setACPCondition(acp, controlplanev1alpha1.PostBootstrapReadyCondition, metav1.ConditionTrue, "", "")
		if err := r.releaseInitLease(ctx, acp); err != nil {
			return err
		}
		return r.reconcileKubeconfig(ctx, acp, cluster)
	}
}
