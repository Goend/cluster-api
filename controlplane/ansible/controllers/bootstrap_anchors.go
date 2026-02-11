package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
)

func (r *AnsibleControlPlaneReconciler) ensureBootstrapAnchors(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) error {
	needsPrimary := acp.Status.ControlPlaneInitialMachine == nil || acp.Status.ControlPlaneInitialMachine.Name == ""
	needsEtcd := len(acp.Status.EtcdInitialMachines) == 0
	if !needsPrimary && !needsEtcd {
		return nil
	}

	machineList := &clusterv1.MachineList{}
	selectors := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	}
	if err := r.Client.List(ctx, machineList, selectors...); err != nil {
		return err
	}

	type candidate struct {
		machine   *clusterv1.Machine
		created   metav1.Time
		hasEtcd   bool
		isControl bool
	}

	configCache := ansibleConfigCache{}
	cpCandidates := []candidate{}
	etcdCandidates := []candidate{}
	for i := range machineList.Items {
		machine := &machineList.Items[i]
		roles, err := r.rolesForMachine(ctx, machine, configCache)
		if err != nil {
			continue
		}
		_, isControl := machine.Labels[clusterv1.MachineControlPlaneLabel]
		cand := candidate{
			machine:   machine,
			created:   machine.CreationTimestamp,
			hasEtcd:   hasRole(roles, "etcd"),
			isControl: isControl,
		}
		if cand.isControl {
			cpCandidates = append(cpCandidates, cand)
		}
		if cand.hasEtcd {
			etcdCandidates = append(etcdCandidates, cand)
		}
	}

	sort.Slice(cpCandidates, func(i, j int) bool {
		if cpCandidates[i].created.Equal(&cpCandidates[j].created) {
			return cpCandidates[i].machine.Name < cpCandidates[j].machine.Name
		}
		return cpCandidates[i].created.Before(&cpCandidates[j].created)
	})
	sort.Slice(etcdCandidates, func(i, j int) bool {
		if etcdCandidates[i].created.Equal(&etcdCandidates[j].created) {
			return etcdCandidates[i].machine.Name < etcdCandidates[j].machine.Name
		}
		return etcdCandidates[i].created.Before(&etcdCandidates[j].created)
	})

	lockHolderName, err := r.currentInitLockHolder(ctx, acp, cluster)
	if err != nil {
		return err
	}

	var holderCandidate *candidate
	if lockHolderName != "" {
		for i := range cpCandidates {
			if cpCandidates[i].machine.Name == lockHolderName {
				holderCandidate = &cpCandidates[i]
				break
			}
		}
	}

	var primary *candidate
	if needsPrimary {
		switch {
		case holderCandidate != nil:
			primary = holderCandidate
			acp.Status.ControlPlaneInitialMachine = objectReferenceFromMachine(primary.machine)
		case len(cpCandidates) > 0:
			primary = &cpCandidates[0]
			acp.Status.ControlPlaneInitialMachine = objectReferenceFromMachine(primary.machine)
		}
	} else if acp.Status.ControlPlaneInitialMachine != nil {
		for i := range cpCandidates {
			if cpCandidates[i].machine.Name == acp.Status.ControlPlaneInitialMachine.Name {
				primary = &cpCandidates[i]
				break
			}
		}
	}

	if needsEtcd {
		requestedEtcd := int32(0)
		if acp.Spec.EtcdReplicas != nil {
			requestedEtcd = *acp.Spec.EtcdReplicas
		}
		if len(etcdCandidates) > 0 {
			limit := len(etcdCandidates)
			if requestedEtcd > 0 && int(requestedEtcd) < limit {
				limit = int(requestedEtcd)
			}
			anchors := make([]corev1.ObjectReference, 0, limit)
			for i := 0; i < limit; i++ {
				if ref := objectReferenceFromMachine(etcdCandidates[i].machine); ref != nil {
					anchors = append(anchors, *ref)
				}
			}
			if len(anchors) > 0 {
				acp.Status.EtcdInitialMachines = anchors
			}
		}
		if len(acp.Status.EtcdInitialMachines) == 0 && requestedEtcd == 0 && primary != nil {
			if ref := objectReferenceFromMachine(primary.machine); ref != nil {
				acp.Status.EtcdInitialMachines = []corev1.ObjectReference{*ref}
			}
		}
	}
	return nil
}

func (r *AnsibleControlPlaneReconciler) currentInitLockHolder(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) (string, error) {
	cmName := fmt.Sprintf("%s-lock", cluster.Name)

	lockInfo, err := r.readConfigMapLock(ctx, cluster.Namespace, cmName)
	if err != nil {
		return "", err
	}
	if lockInfo != "" {
		return lockInfo, nil
	}

	lease := &coordinationv1.Lease{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: acp.Namespace, Name: initLockName(acp)}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if lease.Spec.HolderIdentity != nil {
		return *lease.Spec.HolderIdentity, nil
	}
	return "", nil
}

func (r *AnsibleControlPlaneReconciler) readConfigMapLock(ctx context.Context, namespace, name string) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	data := cm.Data["lock-information"]
	if data == "" {
		return "", nil
	}
	type info struct {
		MachineName string `json:"machineName"`
	}
	lockInfo := info{}
	if err := json.Unmarshal([]byte(data), &lockInfo); err != nil {
		return "", err
	}
	return lockInfo.MachineName, nil
}
