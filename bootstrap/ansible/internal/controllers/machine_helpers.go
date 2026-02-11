package controllers

import (
	"context"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func selectMachineIPAddress(machine *clusterv1.Machine) (string, error) {
	preferred := []clusterv1.MachineAddressType{
		clusterv1.MachineExternalIP,
		clusterv1.MachineInternalIP,
		clusterv1.MachineExternalDNS,
		clusterv1.MachineInternalDNS,
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
