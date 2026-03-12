package controllers

import (
	"context"
	"net"
	"sort"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// selectMachineIPAddress 根据优先级选择 Machine 的地址；
// 当 preferCIDR 非空且可解析时，会优先返回落在该 CIDR 内的第一个地址；
// 若未命中，则回退到原有优先级的不加过滤选择。
func selectMachineIPAddress(machine *clusterv1.Machine, preferCIDR string) (string, error) {
	preferred := []clusterv1.MachineAddressType{
		clusterv1.MachineExternalIP,
		clusterv1.MachineInternalIP,
		clusterv1.MachineExternalDNS,
		clusterv1.MachineInternalDNS,
	}
	// 尝试按 CIDR 过滤优选
	if preferCIDR != "" {
		if _, ipnet, err := net.ParseCIDR(preferCIDR); err == nil && ipnet != nil {
			for _, addressType := range preferred {
				for _, addr := range machine.Status.Addresses {
					if addr.Type != addressType || addr.Address == "" {
						continue
					}
					if ip := net.ParseIP(addr.Address); ip != nil && ipnet.Contains(ip) {
						return addr.Address, nil
					}
				}
			}
		}
	}
	for _, addressType := range preferred {
		for _, addr := range machine.Status.Addresses {
			if addr.Type == addressType && addr.Address != "" {
				return addr.Address, nil
			}
		}
	}
	return "", errors.Errorf("machine %s has no addresses to build inventory", machine.Name)
}

func machineFromReference(ctx context.Context, c client.Client, scope *Scope, ref *corev1.ObjectReference) (*clusterv1.Machine, error) {
	if ref == nil || ref.Name == "" {
		return nil, nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = scope.Cluster.Namespace
	}
	key := types.NamespacedName{Namespace: ns, Name: ref.Name}
	machine := &clusterv1.Machine{}
	if err := c.Get(ctx, key, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return machine, nil
}

func machineFromScope(scope *Scope) (*clusterv1.Machine, error) {
	if scope.ConfigOwner == nil {
		return nil, errors.New("config owner is not set")
	}
	if scope.ConfigOwner.GetKind() != "Machine" {
		return nil, errors.Errorf("%s is not supported for inventory generation", scope.ConfigOwner.GetKind())
	}
	machine := &clusterv1.Machine{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(scope.ConfigOwner.Object, machine); err != nil {
		return nil, errors.Wrap(err, "cannot convert ConfigOwner to Machine")
	}
	return machine, nil
}

// isControlPlane returns true if the Machine has the control-plane label.
func isControlPlane(machine *clusterv1.Machine) bool {
	if machine == nil {
		return false
	}
	_, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

// listSortedControlPlaneMachines 列出指定集群的控制面 Machines，
// 并按 Name 纯字母序升序排序，确保与“首台=名称字母序最小”规则一致且稳定。
func listSortedControlPlaneMachines(ctx context.Context, c client.Client, namespace, clusterName string) ([]*clusterv1.Machine, error) {
	ml := &clusterv1.MachineList{}
	selectors := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName, clusterv1.MachineControlPlaneLabel: ""},
	}
	if err := c.List(ctx, ml, selectors...); err != nil {
		return nil, err
	}
	items := make([]*clusterv1.Machine, 0, len(ml.Items))
	for i := range ml.Items {
		m := ml.Items[i].DeepCopy()
		items = append(items, m)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// firstControlPlaneName 返回名称字母序最小的控制面机器名称；若不存在返回空串。
func firstControlPlaneName(ctx context.Context, c client.Client, namespace, clusterName string) (string, error) {
	cps, err := listSortedControlPlaneMachines(ctx, c, namespace, clusterName)
	if err != nil {
		return "", err
	}
	for _, m := range cps {
		if m == nil {
			continue
		}
		if !m.DeletionTimestamp.IsZero() {
			continue
		}
		return m.Name, nil
	}
	return "", nil
}

// indexOfMachine returns the index of the given machine name in the list, or -1 if not found.
func indexOfMachine(list []*clusterv1.Machine, name string) int {
	for i, m := range list {
		if m != nil && m.Name == name {
			return i
		}
	}
	return -1
}
