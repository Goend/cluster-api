package ansiblecontract

const (
	// ControlPlaneInitialMachineAnnotation marks the first control plane machine selected by the AnsibleControlPlane controller.
	ControlPlaneInitialMachineAnnotation = "controlplane.cluster.x-k8s.io/ansible-initial-machine"

	// EtcdInitialMachineAnnotation marks machines that should participate in etcd bootstrap sequencing.
	EtcdInitialMachineAnnotation = "controlplane.cluster.x-k8s.io/ansible-etcd-anchor"

	annotationTrueValue = "true"
)

// AnnotationTrueValue returns the canonical truthy value used for anchor annotations.
func AnnotationTrueValue() string {
	return annotationTrueValue
}
