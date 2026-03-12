package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NameGenerator is compatible with k8s.io/apiserver/pkg/storage/names.NameGenerator.
// It provides GenerateName(base) string, but this implementation derives a
// fixed-width numeric suffix by scanning existing Machine objects to ensure
// that names are monotonically increasing in lexical order.
type NameGenerator interface {
	GenerateName(base string) string
}

// SequentialMachineNameGenerator generates names of the form `${base}%05d` (默认从 00001 开始) by
// listing existing Machines in the given namespace filtered by cluster label
// and computing max+1 for suffix. Non-conforming names are ignored.
type SequentialMachineNameGenerator struct {
	C         client.Client
	Namespace string
	Cluster   string // cluster name label value to filter Machines
}

const suffixWidth = 5

func NewSequentialMachineNameGenerator(c client.Client, namespace, cluster string) *SequentialMachineNameGenerator {
	return &SequentialMachineNameGenerator{C: c, Namespace: namespace, Cluster: cluster}
}

func (g *SequentialMachineNameGenerator) GenerateName(base string) string {
	// Best-effort: on any list error, fallback to starting from 1.
	ctx := context.Background()
	ml := &clusterv1.MachineList{}
	listOpts := []client.ListOption{client.InNamespace(g.Namespace)}
	if g.Cluster != "" {
		listOpts = append(listOpts, client.MatchingLabels{clusterv1.ClusterNameLabel: g.Cluster})
	}
	_ = g.C.List(ctx, ml, listOpts...)

	max := -1
	prefix := base
	plen := len(prefix)
	for i := range ml.Items {
		name := ml.Items[i].Name
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if len(name) != plen+suffixWidth { // strictly fixed-width numeric suffix
			continue
		}
		suf := name[plen:]
		if n, err := strconv.Atoi(suf); err == nil && n > max {
			max = n
		}
	}
	next := max + 1
	if next < 1 {
		next = 1
	}
	return fmt.Sprintf("%s%0*d", prefix, suffixWidth, next)
}
