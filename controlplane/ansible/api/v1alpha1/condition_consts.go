package v1alpha1

import clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

// Condition types for AnsibleControlPlane.
const (
	// CertificatesAvailableCondition documents that cluster certificates exist and are usable.
	CertificatesAvailableCondition clusterv1.ConditionType = "CertificatesAvailable"

	// KubeconfigAvailableCondition reports whether the admin kubeconfig has been generated.
	KubeconfigAvailableCondition clusterv1.ConditionType = "KubeconfigAvailable"

	// MachinesCreatedCondition ensures Machine objects exist per spec.
	MachinesCreatedCondition clusterv1.ConditionType = "MachinesCreated"

	// PostBootstrapReadyCondition signals that the Ansible post-bootstrap artifacts were prepared.
	PostBootstrapReadyCondition clusterv1.ConditionType = "PostBootstrapReady"

	// KubeanOperationHealthyCondition conveys the health of the delegated Kubean ClusterOperation.
	KubeanOperationHealthyCondition clusterv1.ConditionType = "KubeanOperationHealthy"
)

// Condition reasons surfaced by the controller.
const (
	// CertificatesGenerationFailedReason communicates failures during certificate generation/adoption.
	CertificatesGenerationFailedReason = "CertificatesGenerationFailed"

	// KubeconfigGenerationFailedReason communicates failures while generating kubeconfig Secrets.
	KubeconfigGenerationFailedReason = "KubeconfigGenerationFailed"

	// BootstrapConfigCreationFailedReason surfaces failures while creating AnsibleConfig resources.
	BootstrapConfigCreationFailedReason = "BootstrapConfigCreationFailed"

	// InfrastructureTemplateCloneFailedReason communicates errors while cloning infrastructure templates.
	InfrastructureTemplateCloneFailedReason = "InfrastructureTemplateCloneFailed"

	// MachineCreationFailedReason reports failures persisting Machine objects.
	MachineCreationFailedReason = "MachineCreationFailed"

	// WaitingForClusterInfrastructureReason indicates the Cluster infrastructure is not yet ready.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForControlPlaneEndpointReason indicates the Cluster lacks a valid ControlPlaneEndpoint.
	WaitingForControlPlaneEndpointReason = "WaitingForControlPlaneEndpoint"

	// WaitingForPrimaryMachineReason indicates the first control plane Machine is still initializing.
	WaitingForPrimaryMachineReason = "WaitingForPrimaryMachine"

	// CertificatesAvailableReason indicates cluster certificates are present and valid.
	CertificatesAvailableReason = "CertificatesAvailable"

	// KubeconfigGeneratedReason indicates the admin kubeconfig has been created or updated.
	KubeconfigGeneratedReason = "KubeconfigGenerated"

	// MachinesCreatedReason indicates desired Machine objects have been created.
	MachinesCreatedReason = "MachinesCreated"

	// PostBootstrapCompletedReason indicates Ansible post-bootstrap steps completed.
	PostBootstrapCompletedReason = "PostBootstrapCompleted"
)
