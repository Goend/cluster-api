/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AnsibleConfigSpec defines the desired state of AnsibleConfig.
type AnsibleConfigSpec struct {
	// ClusterRef contains the template required to create a kubean Cluster resource.
	// Deprecated: prefer ClusterTemplate for auto-generated kubean resources.
	// +optional
	ClusterRef ResourceTemplate `json:"clusterRef"`

	// ClusterTemplate defines the parameters required to compose a kubean Cluster resource automatically.
	// +optional
	ClusterTemplate *KubeanClusterTemplate `json:"clusterTemplate,omitempty"`

	// ClusterOpsRef contains the template required to create a kubean ClusterOperation resource.
	// Deprecated: prefer ClusterOperationTemplate for auto-generated kubean resources.
	// +optional
	ClusterOpsRef ResourceTemplate `json:"clusterOpsRef"`

	// ClusterOperationTemplate defines the parameters required to compose a kubean ClusterOperation resource automatically.
	// +optional
	ClusterOperationTemplate *KubeanClusterOperationTemplate `json:"clusterOperationTemplate,omitempty"`

	// CertRef points to the secret containing tls.crt and tls.key which must be baked into the node before ansible runs.
	// +optional
	CertRef corev1.SecretReference `json:"certRef"`

	// Files specifies extra files to be rendered on the target host during cloud-init generation.
	// +optional
	Files []File `json:"files,omitempty"`

	// Role captures the desired Ansible groups for the current node.
	// +optional
	Role []string `json:"role,omitempty"`

	// VarsConfigRefs points to Config CRs that provide overrides for group_vars.yml sections.
	// The "business" and "fixed" references must be provided by the user to supply
	// environment-specific data, while "infra" is optional and defaults to an
	// auto-generated Config managed by the controller when omitted.
	// +kubebuilder:validation:Required
	VarsConfigRefs VarsConfigReferences `json:"varsConfigRefs"`
}

// VarsConfigReferences enumerates the Config resources consumed during vars rendering.
type VarsConfigReferences struct {
	// Business references the user-managed Config containing business-level vars.
	// +kubebuilder:validation:Required
	Business corev1.LocalObjectReference `json:"business"`

	// Fixed references the user-managed Config containing fixed/system vars.
	// +kubebuilder:validation:Required
	Fixed corev1.LocalObjectReference `json:"fixed"`

	// Infra references the (usually auto-generated) Config containing infrastructure facts.
	// +optional
	Infra *corev1.LocalObjectReference `json:"infra,omitempty"`
}

// File describes an additional file rendered by the bootstrap script.
type File struct {
	// Path is the full path where the file should be written.
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Owner defines the file owner in the form user:group. Defaults to root:root.
	// +optional
	Owner string `json:"owner,omitempty"`

	// Permissions is an octal string such as 0600.
	// +optional
	Permissions string `json:"permissions,omitempty"`

	// Content is the inline file contents.
	// +kubebuilder:validation:Required
	Content string `json:"content"`
}

// ResourceTemplate is a generic description of a namespaced resource to be created by the controller.
type ResourceTemplate struct {
	// APIVersion of the target resource, e.g. cluster.kubean.io/v1alpha1.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the target resource, e.g. Cluster or ClusterOperation.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Name of the resource to be created.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the resource. Defaults to the namespace of the AnsibleConfig.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Labels propagated to the created object.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations propagated to the created object.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Spec encodes the spec body that should be copied to the created object.
	// +optional
	Spec runtime.RawExtension `json:"spec,omitempty"`
}

// KubeanClusterTemplate describes the user-overridable pieces of a kubean Cluster resource whose
// metadata (name, namespace, references) are composed automatically by the controller.
type KubeanClusterTemplate struct {
	// APIVersion of the target resource, e.g. cluster.kubean.io/v1alpha1.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the target resource, e.g. Cluster.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace of the resource. Defaults to the namespace of the AnsibleConfig.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Labels propagated to the created object.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations propagated to the created object.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Spec encodes the spec body that should be copied to the created object.
	// +optional
	Spec runtime.RawExtension `json:"spec,omitempty"`
}

// KubeanClusterOperationTemplate describes the overridable pieces of a kubean ClusterOperation resource.
type KubeanClusterOperationTemplate struct {
	// APIVersion of the target resource, e.g. clusterops.kubean.io/v1alpha1.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the target resource, e.g. ClusterOperation.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace of the resource. Defaults to the namespace of the AnsibleConfig.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Labels propagated to the created object.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations propagated to the created object.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Spec encodes the spec body that should be copied to the created object.
	// +optional
	Spec runtime.RawExtension `json:"spec,omitempty"`
}

// AnsibleConfigStatus defines the observed state of AnsibleConfig.
type AnsibleConfigStatus struct {
	// Ready indicates the BootstrapData field is ready to be consumed
	// +optional
	Ready bool `json:"ready"`

	// Initialization mirrors the v1beta2 bootstrap contract status block.
	// When DataSecretCreated=true bootstrap data is guaranteed to exist.
	// +optional
	Initialization *BootstrapDataInitializationStatus `json:"initialization,omitempty"`

	// DataSecretName is the name of the secret that stores the bootstrap data script.
	// +optional
	DataSecretName *string `json:"dataSecretName,omitempty"`

	// FailureReason will be set on non-retryable errors
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage will be set on non-retryable errors
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// ObservedGeneration is the latest generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PostBootstrapCompleted indicates whether the controller finished creating kubean resources.
	// +optional
	PostBootstrapCompleted bool `json:"postBootstrapCompleted,omitempty"`

	// Conditions defines current service state of the AnsibleConfig.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BootstrapDataInitializationStatus reports bootstrap readiness for contract consumers.
type BootstrapDataInitializationStatus struct {
	// DataSecretCreated becomes true once the bootstrap data Secret exists.
	// +optional
	DataSecretCreated bool `json:"dataSecretCreated,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=ansibleconfigs,scope=Namespaced,categories=cluster-api
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']",description="Cluster"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation of AnsibleConfig"

// AnsibleConfig defines the Schema for the Ansible bootstrap configuration API.
type AnsibleConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AnsibleConfigSpec   `json:"spec,omitempty"`
	Status AnsibleConfigStatus `json:"status,omitempty"`
}

// GetConditions returns the set of conditions for this object.
func (c *AnsibleConfig) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions on this object.
func (c *AnsibleConfig) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// AnsibleConfigList contains a list of AnsibleConfig.
type AnsibleConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AnsibleConfig `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &AnsibleConfig{}, &AnsibleConfigList{})
}
