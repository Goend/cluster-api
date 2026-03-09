package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
)

// AnsibleControlPlaneSpec defines the desired state of an Ansible-based control plane.
// +k8s:deepcopy-gen=true
type AnsibleControlPlaneSpec struct {
	// EtcdReplicas is the desired number of etcd Machines. Defaults to 0 which implies a fused control plane/etcd role.
	// Setting this field to a positive value indicates the cluster expects dedicated etcd nodes managed alongside the control plane lifecycle.
	// +optional
	EtcdReplicas *int32 `json:"etcdReplicas,omitempty"`

	// Replicas is the desired number of control plane Machines.
	// Defaults to 1 if not specified.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Version is the Kubernetes version for the control plane nodes.
	// +kubebuilder:validation:MinLength=2
	Version string `json:"version"`

	// MachineTemplate describes the infrastructure used for control plane Machines.
	MachineTemplate clusterv1.MachineTemplateSpec `json:"machineTemplate"`

	// AnsibleConfigSpec carries the bootstrap configuration driven by the Ansible Bootstrap Provider.
	AnsibleConfigSpec bootstrapv1.AnsibleConfigSpec `json:"ansibleConfigSpec"`
}

// AnsibleControlPlaneStatus defines the observed state of AnsibleControlPlane.
// +k8s:deepcopy-gen=true
type AnsibleControlPlaneStatus struct {
	// Selector is the string form of the label selector used for control plane Machines.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Replicas is the total number of non-terminated Machines targeted by this control plane.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Version reports the lowest Kubernetes version among the Ready control plane Machines.
	// +optional
	Version *string `json:"version,omitempty"`

	// UpdatedReplicas is the number of replicas running the desired Machine template.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// ReadyReplicas is the number of Machines targeted by this control plane that have Node Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// AvailableReplicas reports how many replicas meet the Ready/availability requirements.
	// +optional
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`

	// UnavailableReplicas is the difference between desired replicas and ready replicas.
	// +optional
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`

	// ObservedGeneration captures the latest reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentVersion records the version currently enforced on the control plane.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// ControlPlaneInitialMachine references the first control plane Machine that completed bootstrap for this ACP.
	// +optional
	ControlPlaneInitialMachine *corev1.ObjectReference `json:"controlPlaneInitialMachine,omitempty"`

	// EtcdInitialMachines contains the Machine references that should participate in etcd bootstrap sequencing.
	// When etcd and control plane roles are fused, this list mirrors ControlPlaneInitialMachine.
	// +optional
	EtcdInitialMachines []corev1.ObjectReference `json:"etcdInitialMachines,omitempty"`

	// Initialized reports whether at least one control plane instance finished bootstrap.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// Ready indicates the control plane is available to receive requests.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Initialization contains granular initialization status for the control plane.
	// In v1beta2 contract this includes controlPlaneInitialized which signals that
	// the Kubernetes control plane has been initialized and is reachable.
	// +optional
	Initialization ACPInitializationStatus `json:"initialization,omitempty"`

	// FailureReason summarizes terminal reconciliation failures, if any.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage provides human-readable context about failures.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

	// Conditions represents the observations of ACP's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ACPInitializationStatus captures initialization related flags for ACP (v1beta2-style).
type ACPInitializationStatus struct {
	// ControlPlaneInitialized is true when the control plane is initialized and reachable.
	// This mirrors the v1beta2 contract field read by the Cluster controller.
	// +optional
	ControlPlaneInitialized *bool `json:"controlPlaneInitialized,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=ansiblecontrolplanes,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Initialized",type=boolean,JSONPath=".status.initialized",description="Control plane bootstrap completed"
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=".status.ready",description="Control plane ready to serve requests"
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=".spec.replicas",description="Desired control plane replicas",priority=10
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".status.replicas",description="Observed replicas"
// +kubebuilder:printcolumn:name="Ready Replicas",type=integer,JSONPath=".status.readyReplicas",description="Ready replicas"
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=".status.updatedReplicas",description="Updated replicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

// AnsibleControlPlane is the Schema for the ansible control planes API.
type AnsibleControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AnsibleControlPlaneSpec   `json:"spec,omitempty"`
	Status AnsibleControlPlaneStatus `json:"status,omitempty"`
}

// GetConditions returns the ACP status conditions.
func (acp *AnsibleControlPlane) GetConditions() []metav1.Condition {
	return acp.Status.Conditions
}

// SetConditions sets the ACP status conditions.
func (acp *AnsibleControlPlane) SetConditions(conditions []metav1.Condition) {
	acp.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// AnsibleControlPlaneList contains a list of AnsibleControlPlane.
type AnsibleControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AnsibleControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&AnsibleControlPlane{},
		&AnsibleControlPlaneList{},
	)
}
