// Package v1 holds a third party's Volume API. The fleet controller creates
// Volumes, and the third party's controller, which the fleet does not run,
// sets them ready.
// +kubebuilder:object:generate=true
// +groupName=storage.thirdparty
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var GroupVersion = schema.GroupVersion{Group: "storage.thirdparty", Version: "v1"}

var schemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

var AddToScheme = schemeBuilder.AddToScheme

func init() { schemeBuilder.Register(&Volume{}, &VolumeList{}) }

type VolumeSpec struct {
	// +kubebuilder:validation:Enum=small;large
	Size string `json:"size"`
}

type VolumeStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VolumeSpec `json:"spec"`
	// +optional
	Status VolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}
