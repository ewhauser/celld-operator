// Package v1alpha1 contains the provisional, experimental fleet API.
// +kubebuilder:object:generate=true
// +groupName=celld.example.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "celld.example.com", Version: "v1alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &CelldFleet{}, &CelldFleetList{}, &CelldStorageReservation{}, &CelldStorageReservationList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

// CelldFleet supports journaled capacity and maintenance requests. No scale subresource is exposed.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cf
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="fleet name must be a DNS label of at most 40 characters"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type CelldFleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self.qualification == oldSelf.qualification && self.profile == oldSelf.profile && self.serviceAccountName == oldSelf.serviceAccountName && self.storage == oldSelf.storage && self.placement == oldSelf.placement",message="only replicas, capacity, runtimeImage and maintenance may change"
	Spec   CelldFleetSpec   `json:"spec"`
	Status CelldFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.placement.azCount == size(self.placement.zones)",message="azCount must equal the zones count"
// +kubebuilder:validation:XValidation:rule="self.replicas >= self.placement.azCount",message="replicas must be at least azCount"
// +kubebuilder:validation:XValidation:rule="self.profile == 'PersistentFleet' ? has(self.storage.storageClassName) && size(self.storage.storageClassName) > 0 : !has(self.storage.storageClassName)",message="storageClassName is required only for PersistentFleet"
// +kubebuilder:validation:XValidation:rule="self.placement.zones.all(z, z.startsWith(self.storage.region) && size(z) == size(self.storage.region) + 1 && z.matches('.*[a-z]$'))",message="zones must be standard AZ names in storage.region"
// +kubebuilder:validation:XValidation:rule="!has(self.capacity) || self.capacity.minReplicas >= self.placement.azCount",message="capacity minimum must cover requested AZs"
type CelldFleetSpec struct {
	// Requested immutable runtime digest. Omission selects the original v0.5.0 pin.
	// Unsupported transitions are durably blocked, including rollback.
	// +optional
	// +kubebuilder:validation:Pattern=`^ghcr.io/denoland/celld@sha256:[a-f0-9]{64}$`
	RuntimeImage string `json:"runtimeImage,omitempty"`
	// Maintenance requests share the retained lifecycle journal.
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`

	// A required acknowledgment of the qualification boundary; production is unavailable.
	// +kubebuilder:validation:Enum=Experimental
	Qualification string `json:"qualification"`
	// +kubebuilder:validation:Enum=Bucket;PersistentFleet
	Profile string `json:"profile"`
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas int32 `json:"replicas,omitempty"`
	// Optional policy; omission keeps manual ownership.
	Capacity *CapacityPolicy `json:"capacity,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ServiceAccountName string        `json:"serviceAccountName"`
	Storage            StorageSpec   `json:"storage"`
	Placement          PlacementSpec `json:"placement"`
}

// MaintenanceSpec requests suspension or a qualified planned restart.
type MaintenanceSpec struct {
	// Pause new actions and unissued operations; continue recovery of issued actions.
	Paused bool `json:"paused,omitempty"`
	// Change this token to request a planned restart. No restart is qualified yet.
	// +kubebuilder:validation:MaxLength=128
	RestartToken string `json:"restartToken,omitempty"`
}

// One entire bucket is reserved, including all runtime metadata and peer keys.
// Prefix multiplexing and alternate S3 authorities are deliberately unsupported.
type StorageSpec struct {
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	Region string `json:"region"`
	// Required only for PersistentFleet; must reference an existing Retain/WaitForFirstConsumer class.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	StorageClassName string `json:"storageClassName,omitempty"`
	// Disk space in GiB: PVC size for PersistentFleet, disk-backed emptyDir limit for Bucket.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16384
	SizeGiB int32 `json:"sizeGiB,omitempty"`
}

type PlacementSpec struct {
	// Explicit zone allowlist; no automatic reselection when capacity changes.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=6
	// +kubebuilder:validation:items:MaxLength=33
	// +kubebuilder:validation:items:MinLength=1
	// +listType=set
	Zones []string `json:"zones"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=6
	AZCount int32 `json:"azCount"`
	// Relaxed keeps the zone allowlist but changes spread to ScheduleAnyway.
	// +kubebuilder:default=Strict
	// +kubebuilder:validation:Enum=Strict;Relaxed
	Mode string `json:"mode,omitempty"`
}

// LifecycleStatus is an informational projection of the retained reservation journal.
// Clearing status never cancels an operation or removes recovery evidence.
type LifecycleStatus struct {
	// RetiredBucketSessions left logical membership; physical liveness is unknown.
	RetiredBucketSessions int32  `json:"retiredBucketSessions,omitempty"`
	EvidenceBlocker       string `json:"evidenceBlocker,omitempty"`
	EvidenceCheckedAt     string `json:"evidenceCheckedAt,omitempty"`
	SessionCount          int32  `json:"sessionCount,omitempty"`
	Deadline              string `json:"deadline,omitempty"`
	Stalled               bool   `json:"stalled,omitempty"`
	RequestKind           string `json:"requestKind,omitempty"`
	RequestID             string `json:"requestID,omitempty"`
	TargetImage           string `json:"targetImage,omitempty"`
	OperationID           string `json:"operationID,omitempty"`
	Phase                 string `json:"phase,omitempty"`
	From                  int32  `json:"from,omitempty"`
	To                    int32  `json:"to,omitempty"`
	TargetPod             string `json:"targetPod,omitempty"`
	TargetUID             string `json:"targetUID,omitempty"`
	TargetGeneration      string `json:"targetGeneration,omitempty"`
	PossibleLoss          string `json:"possibleLoss,omitempty"`
}

type CelldFleetStatus struct {
	Capacity           CapacityStatus  `json:"capacity,omitempty"`
	Lifecycle          LifecycleStatus `json:"lifecycle,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32           `json:"readyReplicas,omitempty"`
	Reservation        string          `json:"reservation,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type CelldFleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldFleet `json:"items"`
}

// CelldStorageReservation is a durable, cluster-wide tombstone. Never garbage collected.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type CelldStorageReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storage reservations cannot be transferred or changed"
	Spec ReservationSpec `json:"spec"`
}
type ReservationSpec struct {
	// InitialReplicas binds the replica component of SpecHash before provisioning.
	// Optional only for reservations created before this field existed.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	InitialReplicas int32  `json:"initialReplicas,omitempty"`
	Bucket          string `json:"bucket"`
	FleetNamespace  string `json:"fleetNamespace"`
	FleetName       string `json:"fleetName"`
	FleetUID        string `json:"fleetUID"`
	SpecHash        string `json:"specHash"`
}

// +kubebuilder:object:root=true
type CelldStorageReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldStorageReservation `json:"items"`
}
