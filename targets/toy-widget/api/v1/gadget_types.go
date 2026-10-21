package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GadgetSpec struct {
	// Size is the owning Widget's spec.count.
	Size int32 `json:"size"`
}

type GadgetStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Gadget is a dependency of a Widget. Its own controller, which the toy
// does not run, sets its Ready condition.
type Gadget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GadgetSpec `json:"spec"`
	// +optional
	Status GadgetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type GadgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gadget `json:"items"`
}
