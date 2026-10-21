package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GrantSpec struct {
	// Level is the value the Grant's mirror ConfigMap holds.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	Level int32 `json:"level"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Grant has a ClusterRole and a ConfigMap in the mirror namespace, which
// its finalizer deletes.
type Grant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GrantSpec `json:"spec"`
	// +optional
	Status ReadyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type GrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Grant `json:"items"`
}

func init() {
	schemeBuilder.Register(&Grant{}, &GrantList{})
}
