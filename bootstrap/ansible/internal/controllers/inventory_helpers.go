package controllers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/controllers/external"
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

func inventoryNodeFromMachine(machine *clusterv1.Machine, PreferredCIDR string) (inventoryNode, bool) {
	if machine == nil {
		return inventoryNode{}, false
	}
	// 优先按 Scope.PreferredCIDR 过滤选择；失败时回退
	ip, err := selectMachineIPAddress(machine, PreferredCIDR)
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
	// 优先按 Scope.PreferredCIDR 过滤选择；失败时回退
	ip, err := selectMachineIPAddress(machine, scope.PreferredCIDR)
	if err != nil {
		return "", err
	}

	hostNode := newInventoryNode(machine, ip)
	isFirstMasterCluster := scope.ClusterOperationPlan.ActionType == bootstrapv1.ClusterOperationActionCluster

	// Collect hosts: only current host + first init node (primaryAnchor preferred, else first etcd anchor)
	hostEntries := []inventoryNode{hostNode}
	primaryAnchor, etcdAnchors, err := r.resolveInitialInventoryNodes(ctx, scope)
	if err != nil {
		return "", err
	}
	// pick first init node
	var firstInit *inventoryNode
	if primaryAnchor != nil {
		firstInit = primaryAnchor
	} else if len(etcdAnchors) > 0 {
		firstInit = &etcdAnchors[0]
	}
	if firstInit != nil {
		hostEntries = addGroupMember(hostEntries, *firstInit)
	}

	// Collect other initialized control-plane masters (NodeInfo set), excluding firstInit and current host.
	initializedCP := []inventoryNode{}
	if cps, err2 := listSortedControlPlaneMachines(ctx, r.Client, scope.Cluster.Namespace, scope.Cluster.Name); err2 == nil && len(cps) > 0 {
		for _, m := range cps {
			if m == nil || m.Status.NodeInfo == nil {
				continue
			}
			if m.Name == hostNode.Name || (firstInit != nil && m.Name == firstInit.Name) {
				continue
			}
			if node, ok := inventoryNodeFromMachine(m, scope.PreferredCIDR); ok {
				initializedCP = addGroupMember(initializedCP, node)
				hostEntries = addGroupMember(hostEntries, node)
			}
		}
	}

	// Build role membership (reuse existing logic)
	hostRoles := normalizedRoles(scope.Config.Spec.Role)
	groupMembers := map[string][]inventoryNode{}

    // kube-master 组顺序：1) firstInit；2) 已初始化的其他 master；3) 本机（若具备 kube-master 角色）
    if firstInit != nil {
        groupMembers["kube-master"] = addGroupMember(groupMembers["kube-master"], *firstInit)
    }
    if len(initializedCP) > 0 {
        for _, n := range initializedCP {
            groupMembers["kube-master"] = addGroupMember(groupMembers["kube-master"], n)
        }
    }
    if containsRole(hostRoles, "kube-master") {
        groupMembers["kube-master"] = addGroupMember(groupMembers["kube-master"], hostNode)
    }

	// etcd 组顺序：1) firstInit；2) 已初始化的其他 master；3) 本机（若具备 etcd 角色）
	if firstInit != nil {
		groupMembers["etcd"] = addGroupMember(groupMembers["etcd"], *firstInit)
	}
	if len(initializedCP) > 0 {
		for _, n := range initializedCP {
			groupMembers["etcd"] = addGroupMember(groupMembers["etcd"], n)
		}
	}
	if containsRole(hostRoles, "etcd") {
		groupMembers["etcd"] = addGroupMember(groupMembers["etcd"], hostNode)
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

		// 扩容阶段：kube-node 仅包含当前触发调谐的机器（hostNode）
		kubeNodeMembers = addGroupMember(kubeNodeMembers, hostNode)

	}
	groupMembers["kube-node"] = kubeNodeMembers

	// 以手工字符串渲染 YAML，确保组内主机顺序可控（首台 firstInit 在前）。
	fip, _ := r.bastionFIP(ctx, scope)
	return renderInventoryYAML(hostEntries, groupMembers, fip), nil
}

// renderInventoryYAML 以固定顺序输出 YAML，保证 kube-master/etcd 组内首台在前。
func renderInventoryYAML(hostEntries []inventoryNode, groupMembers map[string][]inventoryNode, bastionFIP string) string {
	b := &strings.Builder{}
	wl := func(indent int, s string) {
		b.WriteString(strings.Repeat("\t", 0))
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(s)
		b.WriteString("\n")
	}

	wl(0, "all:")

	// children section first, with stable group order
	wl(2, "children:")
	groupOrder := []string{"etcd", "kube-master", "kube-node", "prometheus", "ingress", "harbor", "nvidia-accelerator", "hygon-accelerator", "ascend-accelerator", "esm", "esm-ingress", "esm-egress"}

	for _, g := range groupOrder {
		nodes := groupMembers[g]
		wl(4, fmt.Sprintf("%s:", g))
		wl(6, "hosts:")
		if len(nodes) == 0 {
			wl(8, "{}")
			continue
		}
		for _, n := range nodes {
			if n.Name == "" {
				continue
			}
			wl(8, fmt.Sprintf("%s: {}", n.Name))
		}
	}

	// k8s-cluster group
	wl(4, "k8s-cluster:")
	wl(6, "children:")
	wl(8, "kube-master: {}")
	wl(8, "kube-node: {}")

	// hosts section
	wl(2, "hosts:")
	seen := map[string]struct{}{}
	if bastionFIP != "" {
		wl(4, "bastion:")
		wl(6, fmt.Sprintf("ansible_host: %s", bastionFIP))
		wl(6, fmt.Sprintf("ansible_ssh_host: %s", bastionFIP))
		wl(6, "ansible_user: root")
	}
	for _, n := range hostEntries {
		if n.Name == "" || n.IP == "" {
			continue
		}
		if _, ok := seen[n.Name]; ok {
			continue
		}
		seen[n.Name] = struct{}{}
		wl(4, fmt.Sprintf("%s:", n.Name))
		wl(6, fmt.Sprintf("access_ip: %s", n.IP))
		wl(6, fmt.Sprintf("ansible_host: %s", n.IP))
		wl(6, fmt.Sprintf("ansible_ssh_host: %s", n.IP))
		wl(6, fmt.Sprintf("ip: %s", n.IP))
	}
	return b.String()
}

// bastionFIP tries to read bastion Floating IP from the infrastructure cluster object.
func (r *AnsibleConfigReconciler) bastionFIP(ctx context.Context, scope *Scope) (string, error) {
	if scope.Cluster == nil || !scope.Cluster.Spec.InfrastructureRef.IsDefined() {
		return "", nil
	}
	infraObj, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
	if err != nil || infraObj == nil {
		return "", err
	}
	obj := infraObj.UnstructuredContent()
	// Priority 1: public Floating IP
	if v, found, _ := unstructured.NestedString(obj, "status", "bastion", "floatingIP"); found && v != "" {
		return v, nil
	}
	// Priority 2: internal IP (layer-2 reachable environments)
	if v, found, _ := unstructured.NestedString(obj, "status", "bastion", "ip"); found && v != "" {
		return v, nil
	}
	return "", nil
}

func (r *AnsibleConfigReconciler) resolveInitialInventoryNodes(ctx context.Context, scope *Scope) (*inventoryNode, []inventoryNode, error) {
	primaryMachine, etcdMachines, err := r.findAnchorMachines(ctx, scope)
	if err != nil {
		return nil, nil, err
	}

	var controlPlaneNode *inventoryNode
	if node, ok := inventoryNodeFromMachine(primaryMachine, scope.PreferredCIDR); ok {
		controlPlaneNode = &node
	}

	etcdNodes := make([]inventoryNode, 0, len(etcdMachines))
	for _, machine := range etcdMachines {
		if node, ok := inventoryNodeFromMachine(machine, scope.PreferredCIDR); ok {
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
