// Package v1 holds the API of the toy fleet: Clusters, the Databases they
// host, and the Users of those Databases.
// +kubebuilder:object:generate=true
// +groupName=toy.reconciler-fuzzer
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group and version this package serves.
var GroupVersion = schema.GroupVersion{Group: "toy.reconciler-fuzzer", Version: "v1"}

var schemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers the fleet types with a scheme.
var AddToScheme = schemeBuilder.AddToScheme

func init() {
	schemeBuilder.Register(&Cluster{}, &ClusterList{}, &Database{}, &DatabaseList{}, &User{}, &UserList{})
}

// Phase is where a CR is in its lifecycle.
type Phase string

const (
	Pending      Phase = "Pending"
	Provisioning Phase = "Provisioning"
	Ready        Phase = "Ready"
	Deleting     Phase = "Deleting"
)

// CommonStatus is every fleet kind's status, so one ready expression fits
// all three.
type CommonStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// ObservedGeneration is the generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Message says what the CR waits for.
	// +optional
	Message string `json:"message,omitempty"`
}

// ReadyAt says whether s reports Ready for generation.
func (s CommonStatus) ReadyAt(generation int64) bool {
	return s.Phase == Ready && s.ObservedGeneration == generation
}
