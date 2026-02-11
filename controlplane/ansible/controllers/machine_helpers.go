package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	controlplanev1alpha1 "sigs.k8s.io/cluster-api/controlplane/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/conditions"
)

type ansibleConfigCache map[client.ObjectKey]*bootstrapv1.AnsibleConfig

var ansibleConfigGroupKind = bootstrapv1.GroupVersion.WithKind("AnsibleConfig").GroupKind()

func (r *AnsibleControlPlaneReconciler) listControlPlaneMachines(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) (*clusterv1.MachineList, error) {
	selectorLabels := labels.Set{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}
	machineList := &clusterv1.MachineList{}
	listOpts := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels(selectorLabels),
	}
	if err := r.Client.List(ctx, machineList, listOpts...); err != nil {
		return nil, err
	}
	return machineList, nil
}

func selectPrimaryMachine(machines []clusterv1.Machine, preferred *corev1.ObjectReference) *clusterv1.Machine {
	if preferred != nil && preferred.Name != "" {
		for i := range machines {
			if machines[i].Name == preferred.Name {
				return &machines[i]
			}
		}
	}
	var chosen *clusterv1.Machine
	for i := range machines {
		if chosen == nil {
			chosen = &machines[i]
			continue
		}
		if machines[i].CreationTimestamp.Before(&chosen.CreationTimestamp) {
			chosen = &machines[i]
			continue
		}
		if machines[i].CreationTimestamp.Equal(&chosen.CreationTimestamp) && machines[i].Name < chosen.Name {
			chosen = &machines[i]
		}
	}
	return chosen
}

func (r *AnsibleControlPlaneReconciler) primaryMachineProgress(ctx context.Context, machine *clusterv1.Machine, cache ansibleConfigCache) (bool, bool, error) {
	if machine == nil {
		return false, false, nil
	}
	cfg, err := r.ansibleConfigForMachine(ctx, machine, cache)
	if err != nil {
		return false, false, err
	}
	postBootstrap := false
	if cfg != nil {
		postBootstrap = cfg.Status.PostBootstrapCompleted
	}
	nodeReady := machine.Status.NodeRef.IsDefined() && conditions.IsTrue(machine, string(clusterv1.ReadyCondition))
	return postBootstrap, nodeReady, nil
}

func (r *AnsibleControlPlaneReconciler) ansibleConfigForMachine(ctx context.Context, machine *clusterv1.Machine, cache ansibleConfigCache) (*bootstrapv1.AnsibleConfig, error) {
	if machine == nil {
		return nil, nil
	}
	ref := machine.Spec.Bootstrap.ConfigRef
	if !ref.IsDefined() || ref.GroupKind() != ansibleConfigGroupKind {
		return nil, nil
	}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}
	if cfg, found := cache[key]; found {
		return cfg, nil
	}
	cfg := &bootstrapv1.AnsibleConfig{}
	if err := r.Client.Get(ctx, key, cfg); err != nil {
		return nil, err
	}
	cache[key] = cfg
	return cfg, nil
}

func buildMachineLabels(acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) map[string]string {
	labels := map[string]string{}
	for k, v := range acp.Spec.MachineTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	labels[clusterv1.ClusterNameLabel] = cluster.Name
	labels[clusterv1.MachineControlPlaneLabel] = ""
	return labels
}

func buildMachineAnnotations(acp *controlplanev1alpha1.AnsibleControlPlane) map[string]string {
	annotations := map[string]string{}
	for k, v := range acp.Spec.MachineTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}
	return annotations
}

func desiredEtcdReplicas(acp *controlplanev1alpha1.AnsibleControlPlane) int32 {
	if acp.Spec.EtcdReplicas == nil {
		return 0
	}
	if *acp.Spec.EtcdReplicas < 0 {
		return 0
	}
	return *acp.Spec.EtcdReplicas
}

func buildEtcdMachineLabels(acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) map[string]string {
	labels := buildMachineLabels(acp, cluster)
	delete(labels, clusterv1.MachineControlPlaneLabel)
	labels[etcdMachineLabel] = ""
	return labels
}

func (r *AnsibleControlPlaneReconciler) listEtcdMachines(ctx context.Context, acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster) (*clusterv1.MachineList, error) {
	selectorLabels := labels.Set{
		clusterv1.ClusterNameLabel: cluster.Name,
		etcdMachineLabel:           "",
	}
	machineList := &clusterv1.MachineList{}
	listOpts := []client.ListOption{
		client.InNamespace(acp.Namespace),
		client.MatchingLabels(selectorLabels),
	}
	if err := r.Client.List(ctx, machineList, listOpts...); err != nil {
		return nil, err
	}
	return machineList, nil
}

func (r *AnsibleControlPlaneReconciler) rolesForMachine(ctx context.Context, machine *clusterv1.Machine, cache ansibleConfigCache) ([]string, error) {
	cfg, err := r.ansibleConfigForMachine(ctx, machine, cache)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return cfg.Spec.Role, nil
}

func hasRole(roles []string, target string) bool {
	for _, role := range roles {
		if role == target {
			return true
		}
	}
	return false
}

func (r *AnsibleControlPlaneReconciler) ensureMachineRole(ctx context.Context, machine *clusterv1.Machine, cache ansibleConfigCache, role string) error {
	if machine == nil || role == "" {
		return nil
	}
	cfg, err := r.ansibleConfigForMachine(ctx, machine, cache)
	if err != nil || cfg == nil {
		return err
	}
	if hasRole(cfg.Spec.Role, role) {
		return nil
	}
	cfg.Spec.Role = append(cfg.Spec.Role, role)
	return r.Client.Update(ctx, cfg)
}

func objectReferenceFromMachine(machine *clusterv1.Machine) *corev1.ObjectReference {
	if machine == nil {
		return nil
	}
	return &corev1.ObjectReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Machine",
		Namespace:  machine.Namespace,
		Name:       machine.Name,
		UID:        machine.UID,
	}
}

func initLockName(acp *controlplanev1alpha1.AnsibleControlPlane) string {
	return fmt.Sprintf("%s-%s", acp.Name, initLockNameSuffix)
}

func buildMachineSpec(acp *controlplanev1alpha1.AnsibleControlPlane, cluster *clusterv1.Cluster, infraRef clusterv1.ContractVersionedObjectReference, bootstrapRef clusterv1.ContractVersionedObjectReference) clusterv1.MachineSpec {
	spec := acp.Spec.MachineTemplate.Spec.DeepCopy()
	spec.ClusterName = cluster.Name
	spec.Version = acp.Spec.Version
	spec.InfrastructureRef = infraRef
	spec.Bootstrap.ConfigRef = bootstrapRef
	spec.Bootstrap.DataSecretName = nil
	return *spec
}
