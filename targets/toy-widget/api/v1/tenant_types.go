package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type TenantSpec struct {
	// Quota is the label value each child of the Tenant carries.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	Quota int32 `json:"quota"`
}

// ReadyStatus is the status of a Tenant or a Grant.
type ReadyStatus struct {
	// ObservedGeneration is the generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration"`
	// Ready says whether every child exists and matches the spec.
	// +optional
	Ready bool `json:"ready"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status

// Tenant owns a Namespace and a ClusterRole named after it.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TenantSpec `json:"spec"`
	// +optional
	Status ReadyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	schemeBuilder.Register(&Tenant{}, &TenantList{})
}
