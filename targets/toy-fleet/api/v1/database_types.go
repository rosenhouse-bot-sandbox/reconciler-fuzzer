package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DatabaseFinalizer holds a Database until its Users are gone.
const DatabaseFinalizer = "fleet.toy/db"

type DatabaseSpec struct {
	// ClusterRef names the Cluster, in the Database's namespace, that hosts it.
	// +kubebuilder:validation:MinLength=1
	ClusterRef string `json:"clusterRef"`
	// +kubebuilder:validation:Enum=postgres;mysql
	Engine string `json:"engine"`
	// PasswordSecretRef names a Secret, in the Database's namespace, whose
	// data holds the admin password.
	// +optional
	PasswordSecretRef *corev1.LocalObjectReference `json:"passwordSecretRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Database is ready once its Cluster is. It owns a ConfigMap.
type Database struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DatabaseSpec `json:"spec"`
	// +optional
	Status CommonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Database `json:"items"`
}
