// Package v1 holds the API of the bucket toy.
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

// AddToScheme registers the Bucket types with a scheme.
var AddToScheme = schemeBuilder.AddToScheme

// CreatePending is the condition --halt sets after a create of unknown outcome.
const CreatePending = "CreatePending"

type BucketSpec struct {
	// Region is where the bucket lives. A change patches the bucket.
	// +kubebuilder:validation:Enum=eu;us
	Region string `json:"region"`
}

type BucketStatus struct {
	// Ready says the bucket is READY in the Bucket's region.
	// +optional
	Ready bool `json:"ready"`
	// BucketID is the cloud's id of the bucket.
	// +optional
	BucketID string `json:"bucketID,omitempty"`
	// ObservedGeneration is the Bucket generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Bucket owns one bucket in the cloud.
type Bucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BucketSpec `json:"spec"`
	// +optional
	Status BucketStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type BucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Bucket `json:"items"`
}

func init() {
	schemeBuilder.Register(&Bucket{}, &BucketList{})
}
