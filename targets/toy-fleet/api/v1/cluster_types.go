package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterFinalizer holds a Cluster until its Databases and Volumes are gone.
const ClusterFinalizer = "fleet.toy/cleanup"

type ClusterSpec struct {
	// Volumes is how many Volumes the Cluster owns.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Volumes int32 `json:"volumes"`
	// VolumeSize is each Volume's spec.size.
	// +kubebuilder:validation:Enum=small;large
	VolumeSize string `json:"volumeSize"`
}

type ClusterStatus struct {
	CommonStatus `json:",inline"`
	// ReadyVolumes counts the Volumes ready for their current generation.
	// +optional
	ReadyVolumes int32 `json:"readyVolumes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// Cluster is Pending at first, Provisioning while any of its Volumes is not
// ready, Ready while all are, and Deleting once deleted. It owns spec.volumes
// Volumes, whose third-party controller sets them ready.
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterSpec `json:"spec"`
	// +optional
	Status ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}
