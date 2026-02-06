package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AnsibleControlPlaneTemplateSpec defines the desired state of AnsibleControlPlane in a ClusterClass.
// +k8s:deepcopy-gen=true
type AnsibleControlPlaneTemplateSpec struct {
	Template AnsibleControlPlaneTemplateResource `json:"template"`
}

// AnsibleControlPlaneTemplateResource describes the data needed to create a control plane from a template.
// +k8s:deepcopy-gen=true
type AnsibleControlPlaneTemplateResource struct {
	Spec AnsibleControlPlaneSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=ansiblecontrolplanetemplates,scope=Namespaced,categories=cluster-api

// AnsibleControlPlaneTemplate is the Schema for the control plane templates API.
type AnsibleControlPlaneTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AnsibleControlPlaneTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// AnsibleControlPlaneTemplateList contains a list of templates.
type AnsibleControlPlaneTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AnsibleControlPlaneTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&AnsibleControlPlaneTemplate{},
		&AnsibleControlPlaneTemplateList{},
	)
}
