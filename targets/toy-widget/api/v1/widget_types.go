// Package v1 holds the API of the toy target.
// +kubebuilder:object:generate=true
// +groupName=toy.reconciler-fuzzer
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group and version this package serves.
var GroupVersion = schema.GroupVersion{Group: "toy.reconciler-fuzzer", Version: "v1"}

var schemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers the Widget types with a scheme.
var AddToScheme = schemeBuilder.AddToScheme

type WidgetSpec struct {
	// Count is how many ConfigMaps the Widget requires.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	Count int32 `json:"count"`
}

type WidgetStatus struct {
	// Ready is how many of those ConfigMaps are present.
	// +optional
	Ready int32 `json:"ready"`
	// ObservedGeneration is the Widget generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration"`
	// LastSyncTime is when the controller last reconciled. Only the seeded bug
	// B6 writes it, and it holds microseconds so that every write lands as a
	// change.
	// +optional
	LastSyncTime *metav1.MicroTime `json:"lastSyncTime,omitempty"`
	// GadgetReady says whether the Widget's Gadget is ready. Only --gadget
	// writes it.
	// +optional
	GadgetReady *bool `json:"gadgetReady,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Widget owns one ConfigMap per index below spec.count.
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WidgetSpec `json:"spec"`
	// +optional
	Status WidgetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

func init() {
	schemeBuilder.Register(&Widget{}, &WidgetList{}, &Gadget{}, &GadgetList{})
}
