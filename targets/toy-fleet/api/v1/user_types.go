package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type UserSpec struct {
	// DatabaseRef names the Database, in the User's namespace, the User logs in to.
	// +kubebuilder:validation:MinLength=1
	DatabaseRef string `json:"databaseRef"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// User is ready once its Database is. It owns a credentials Secret, and has
// no finalizer.
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UserSpec `json:"spec"`
	// +optional
	Status CommonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}
