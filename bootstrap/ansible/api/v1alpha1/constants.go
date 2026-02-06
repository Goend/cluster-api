package v1alpha1

const (
	// ClusterOperationActionAnnotation stores the kubean ClusterOperation action type requested for a node.
	ClusterOperationActionAnnotation = "bootstrap.cluster.x-k8s.io/operation-action"
	// ClusterOperationScaleMasterAnnotation indicates whether the current scale action targets a master node.
	ClusterOperationScaleMasterAnnotation = "bootstrap.cluster.x-k8s.io/scale-master"

	// ClusterOperationActionCluster is the default action for the first control plane machine.
	ClusterOperationActionCluster = "cluster.yml"
	// ClusterOperationActionScale is the default action for scaling subsequent machines.
	ClusterOperationActionScale = "scale.yml"
)
