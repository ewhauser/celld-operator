package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ApplicationVersion identifies deployment artifacts, independently of runtime
// image pins, Kubernetes generations and process-local application generations.
type ApplicationVersion struct {
	// +kubebuilder:validation:MaxLength=256
	Version string `json:"version"`
	// +kubebuilder:validation:MaxLength=1024
	Prefix string `json:"prefix"`
}
type ApplicationVersionCount struct {
	ApplicationVersion `json:",inline"`
	Nodes              int32 `json:"nodes"`
}
type ApplicationNodeStatus struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Version string `json:"version,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
}

// ApplicationStatus describes the last bounded observation of current fleet
// membership. Convergence is relative to observed deployment pointers, not an
// assertion that a newer deployment has not been published since the poll.
type ApplicationStatus struct {
	ObservedAt       metav1.Time `json:"observedAt"`
	ExpectedNodes    int32       `json:"expectedNodes"`
	ObservedNodes    int32       `json:"observedNodes"`
	UnavailableNodes int32       `json:"unavailableNodes"`
	// Common freshly observed target; omitted when targets disagree or coverage is incomplete.
	// +optional
	Target *ApplicationVersion `json:"target,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Versions []ApplicationVersionCount `json:"versions,omitempty"`
	// Counts from valid fresh observations only; incomplete coverage is Unknown.
	PendingCells  int64 `json:"pendingCells"`
	SwappingCells int64 `json:"swappingCells"`
	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	Nodes []ApplicationNodeStatus `json:"nodes,omitempty"`
}
