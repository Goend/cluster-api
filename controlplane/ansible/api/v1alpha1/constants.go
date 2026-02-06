package v1alpha1

const (
	// AnsibleControlPlaneFinalizer ensures cleanup happens before deleting the resource.
	AnsibleControlPlaneFinalizer = "ansiblecontrolplane.controlplane.cluster.x-k8s.io"

	// ImplementingReason communicates that the controller still lacks business logic.
	ImplementingReason = "ImplementationPending"

	// UpgradeUnsupportedReason indicates that in-place upgrades are not yet supported.
	UpgradeUnsupportedReason = "UpgradeNotSupported"
)
