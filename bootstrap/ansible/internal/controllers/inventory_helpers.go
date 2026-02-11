package controllers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/internal/ansiblecontract"
)

type inventoryNode struct {
	Name    string
	IP      string
	orderBy string
}

var errAnchorMachinesUnavailable = errors.New("ansible anchor machines not ready")

func formatInventoryHosts(nodes []inventoryNode) []string {
	lines := []string{}
	seen := map[string]struct{}{}
	for _, node := range nodes {
		if node.Name == "" || node.IP == "" {
			continue
		}
		key := fmt.Sprintf("%s|%s", node.Name, node.IP)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		lines = append(lines, fmt.Sprintf("%s ansible_ssh_host=%s   ip=%s", node.Name, node.IP, node.IP))
	}
	return lines
}

func formatInventoryGroup(group string, nodes []inventoryNode, placeholder string) []string {
	lines := []string{
		fmt.Sprintf("[%s]", group),
	}
	if len(nodes) == 0 && placeholder != "" {
		lines = append(lines, placeholder)
	} else {
		for _, node := range nodes {
			if node.Name == "" {
				continue
			}
			lines = append(lines, node.Name)
		}
	}
	lines = append(lines, "")
	return lines
}

func normalizedRoles(roles []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		if _, exists := seen[role]; exists {
			continue
		}
		seen[role] = struct{}{}
		result = append(result, role)
	}
	sort.Strings(result)
	return result
}

func containsRole(roles []string, target string) bool {
	for _, role := range roles {
		if role == target {
			return true
		}
	}
	return false
}

func newInventoryNode(machine *clusterv1.Machine, ip string) inventoryNode {
	orderKey := fmt.Sprintf("%s/%s", machine.CreationTimestamp.UTC().Format(time.RFC3339Nano), machine.Name)
	return inventoryNode{
		Name:    machine.Name,
		IP:      ip,
		orderBy: orderKey,
	}
}

func inventoryNodeFromMachine(machine *clusterv1.Machine) (inventoryNode, bool) {
	if machine == nil {
		return inventoryNode{}, false
	}
	ip, err := selectMachineIPAddress(machine)
	if err != nil || ip == "" {
		return inventoryNode{}, false
	}
	return inventoryNode{Name: machine.Name, IP: ip}, true
}

func addGroupMember(nodes []inventoryNode, node inventoryNode) []inventoryNode {
	for _, existing := range nodes {
		if existing.Name == node.Name {
			return nodes
		}
	}
	return append(nodes, node)
}

func filterInventoryNodes(nodes []inventoryNode, keep func(inventoryNode) bool) []inventoryNode {
	if keep == nil {
		return nodes
	}
	filtered := make([]inventoryNode, 0, len(nodes))
	for _, node := range nodes {
		if keep(node) {
			filtered = append(filtered, node)
		}
	}
	return filtered
}

func (r *AnsibleConfigReconciler) buildHostsInventory(ctx context.Context, scope *Scope) (string, error) {
	machine, err := machineFromScope(scope)
	if err != nil {
		return "", err
	}
	ip, err := selectMachineIPAddress(machine)
	if err != nil {
		return "", err
	}

	hostNode := newInventoryNode(machine, ip)
	isFirstMasterCluster := scope.ClusterOperationPlan.ActionType == bootstrapv1.ClusterOperationActionCluster
	lines := []string{
		"## Configure 'ip' variable to bind kubernetes services on a",
		"## different ip than the default iface",
	}
	hostEntries := []inventoryNode{hostNode}
	primaryAnchor, etcdAnchors, err := r.resolveInitialInventoryNodes(ctx, scope)
	if err != nil {
		return "", err
	}
	if primaryAnchor != nil {
		hostEntries = addGroupMember(hostEntries, *primaryAnchor)
	}
	for _, node := range etcdAnchors {
		hostEntries = addGroupMember(hostEntries, node)
	}
	lines = append(lines, formatInventoryHosts(hostEntries)...)
	lines = append(lines,
		"",
		"# configure a bastion host if your nodes are not directly reachable",
		"# bastion ansible_ssh_host=x.x.x.x",
		"",
	)

	hostRoles := normalizedRoles(scope.Config.Spec.Role)
	groupMembers := map[string][]inventoryNode{}

	if containsRole(hostRoles, "kube-master") {
		groupMembers["kube-master"] = addGroupMember(groupMembers["kube-master"], hostNode)
	}
	if primaryAnchor != nil {
		groupMembers["kube-master"] = addGroupMember(groupMembers["kube-master"], *primaryAnchor)
	}

	if containsRole(hostRoles, "etcd") {
		groupMembers["etcd"] = addGroupMember(groupMembers["etcd"], hostNode)
	}
	for _, node := range etcdAnchors {
		groupMembers["etcd"] = addGroupMember(groupMembers["etcd"], node)
	}

	for _, role := range hostRoles {
		if role == "kube-master" || role == "etcd" {
			continue
		}
		groupMembers[role] = addGroupMember(groupMembers[role], hostNode)
	}

	kubeNodeMembers := groupMembers["kube-node"]
	if isFirstMasterCluster {
		kubeNodeMembers = filterInventoryNodes(kubeNodeMembers, func(node inventoryNode) bool {
			if node.Name == hostNode.Name {
				return false
			}
			for _, etcdNode := range etcdAnchors {
				if node.Name == etcdNode.Name {
					return false
				}
			}
			return true
		})
	} else {
		kubeNodeMembers = addGroupMember(kubeNodeMembers, hostNode)
		if primaryAnchor != nil {
			kubeNodeMembers = addGroupMember(kubeNodeMembers, *primaryAnchor)
		}
		for _, etcdNode := range etcdAnchors {
			kubeNodeMembers = addGroupMember(kubeNodeMembers, etcdNode)
		}
	}
	groupMembers["kube-node"] = kubeNodeMembers

	groupNames := make([]string, 0, len(groupMembers))
	for group, nodes := range groupMembers {
		if len(nodes) == 0 {
			continue
		}
		groupNames = append(groupNames, group)
		lines = append(lines, formatInventoryGroup(group, nodes, "# populated by ansible controlplane")...)
	}

	sort.Strings(groupNames)
	childNodes := make([]inventoryNode, 0, len(groupNames))
	for _, group := range groupNames {
		childNodes = append(childNodes, inventoryNode{Name: group})
	}
	lines = append(lines, formatInventoryGroup("k8s-cluster:children", childNodes, "")...)
	return strings.Join(lines, "\n"), nil
}

func (r *AnsibleConfigReconciler) resolveInitialInventoryNodes(ctx context.Context, scope *Scope) (*inventoryNode, []inventoryNode, error) {
	primaryMachine, etcdMachines, err := r.findAnchorMachines(ctx, scope)
	if err != nil {
		return nil, nil, err
	}

	var controlPlaneNode *inventoryNode
	if node, ok := inventoryNodeFromMachine(primaryMachine); ok {
		controlPlaneNode = &node
	}

	etcdNodes := make([]inventoryNode, 0, len(etcdMachines))
	for _, machine := range etcdMachines {
		if node, ok := inventoryNodeFromMachine(machine); ok {
			etcdNodes = addGroupMember(etcdNodes, node)
		}
	}

	if len(etcdNodes) == 0 && controlPlaneNode != nil {
		etcdNodes = addGroupMember(etcdNodes, *controlPlaneNode)
	}

	return controlPlaneNode, etcdNodes, nil
}

func (r *AnsibleConfigReconciler) findAnchorMachines(ctx context.Context, scope *Scope) (*clusterv1.Machine, []*clusterv1.Machine, error) {
	machineList := &clusterv1.MachineList{}
	selectors := []client.ListOption{
		client.InNamespace(scope.Cluster.Namespace),
		client.MatchingLabels{
			clusterv1.ClusterNameLabel: scope.Cluster.Name,
		},
	}
	if err := r.Client.List(ctx, machineList, selectors...); err != nil {
		return nil, nil, err
	}

	var primary *clusterv1.Machine
	etcdMachines := []*clusterv1.Machine{}

	for i := range machineList.Items {
		machine := machineList.Items[i].DeepCopy()
		annotations := machine.GetAnnotations()
		if annotations[ansiblecontract.ControlPlaneInitialMachineAnnotation] == ansiblecontract.AnnotationTrueValue() {
			primary = machine
		}
		if annotations[ansiblecontract.EtcdInitialMachineAnnotation] == ansiblecontract.AnnotationTrueValue() {
			etcdMachines = append(etcdMachines, machine)
		}
	}

	if primary == nil && len(etcdMachines) == 0 {
		return nil, nil, errAnchorMachinesUnavailable
	}

	return primary, etcdMachines, nil
}
