// Package v1alpha1 contains the experimental fleet API under the celld.eric.dev group.
// +kubebuilder:object:generate=true
// +groupName=celld.eric.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "celld.eric.dev", Version: "v1alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &CelldFleet{}, &CelldFleetList{}, &CelldStorageReservation{}, &CelldStorageReservationList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

// CelldFleet supports serialized capacity and maintenance requests. The /scale
// subresource declared below exposes spec.replicas for external capacity mode.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cf
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.labelSelector
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="fleet name must be a DNS label of at most 40 characters"
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type CelldFleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self.qualification == oldSelf.qualification && self.profile == oldSelf.profile && self.serviceAccountName == oldSelf.serviceAccountName && self.storage == oldSelf.storage && self.placement == oldSelf.placement && has(self.execution) == has(oldSelf.execution) && (!has(self.execution) || self.execution == oldSelf.execution) && has(self.lifecycle) == has(oldSelf.lifecycle) && (!has(self.lifecycle) || self.lifecycle == oldSelf.lifecycle) && self.bucketWorkload == oldSelf.bucketWorkload",message="only replicas, capacity, runtimeImage and maintenance may change; layout, execution and lifecycle tuning are fixed at creation"
	Spec   CelldFleetSpec   `json:"spec"`
	Status CelldFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.bucketWorkload != 'Ordered' || self.profile == 'Bucket'",message="Ordered bucketWorkload requires Bucket profile"
// +kubebuilder:validation:XValidation:rule="self.placement.azCount == size(self.placement.zones)",message="azCount must equal the zones count"
// +kubebuilder:validation:XValidation:rule="self.replicas >= self.placement.azCount",message="replicas must be at least azCount"
// +kubebuilder:validation:XValidation:rule="self.profile == 'PersistentFleet' ? has(self.storage.storageClassName) && size(self.storage.storageClassName) > 0 : !has(self.storage.storageClassName)",message="storageClassName is required only for PersistentFleet"
// +kubebuilder:validation:XValidation:rule="self.placement.zones.all(z, z.startsWith(self.storage.region) && size(z) == size(self.storage.region) + 1 && z.matches('.*[a-z]$'))",message="zones must be standard AZ names in storage.region"
// +kubebuilder:validation:XValidation:rule="!has(self.capacity) || self.capacity.minReplicas >= self.placement.azCount",message="capacity minimum must cover requested AZs"
type CelldFleetSpec struct {
	// Bucket workload layout. Defaults to Deployment; Ordered uses deterministic ordinal removal. Layout is immutable.
	// +kubebuilder:default=Deployment
	// +kubebuilder:validation:Enum=Deployment;Ordered
	BucketWorkload string `json:"bucketWorkload,omitempty"`
	// Requested immutable runtime digest. Required for provisioning; no default release is assumed.
	// Use a homogeneous compatible fork; release and recovery qualification remain required.
	// +optional
	// +kubebuilder:validation:Pattern=`^ghcr.io/ewhauser/celld@sha256:[a-f0-9]{64}$`
	RuntimeImage string `json:"runtimeImage,omitempty"`
	// Maintenance requests share the bounded current operation.
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`

	// A required acknowledgment of the qualification boundary; production is unavailable.
	// +kubebuilder:validation:Enum=Experimental
	Qualification string `json:"qualification"`
	// Storage profile: Bucket uses temporary local disk and S3; PersistentFleet adds peer disks disposed after strict shutdown. Immutable after creation.
	// +kubebuilder:validation:Enum=Bucket;PersistentFleet
	Profile string `json:"profile"`
	// Manual replica target. Capacity policy can choose a different applied count without editing this field. Must cover every configured availability zone.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas int32 `json:"replicas,omitempty"`
	// Optional policy; omission keeps manual ownership.
	Capacity *CapacityPolicy `json:"capacity,omitempty"`
	// Existing ServiceAccount in the fleet namespace with the runtime bucket permissions. Immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ServiceAccountName string `json:"serviceAccountName"`
	// Dedicated bucket and local disk settings. Immutable after creation.
	Storage StorageSpec `json:"storage"`
	// Allowed availability zones and scheduling strictness. Immutable after creation.
	Placement PlacementSpec `json:"placement"`
	// Per-fleet runtime sizing. Immutable after creation: the operator never rolls
	// out a changed pod template. Omitted fields keep the prototype constants.
	// +optional
	Execution *ExecutionSpec `json:"execution,omitempty"`
	// Per-fleet shutdown and termination budgets. Immutable after creation.
	// +optional
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`
}

// ExecutionSpec sizes the celld container and bounds its residency. Values are
// starting points, not capacity guarantees; the capacity policy's thresholds are
// absolute and independent of these requests.
// +kubebuilder:validation:XValidation:rule="!has(self.cpuLimit) || !has(self.cpuRequest) || quantity(self.cpuLimit).isGreaterThan(quantity(self.cpuRequest)) || quantity(self.cpuLimit).compareTo(quantity(self.cpuRequest)) == 0",message="cpuLimit must be at least cpuRequest"
// +kubebuilder:validation:XValidation:rule="!has(self.memoryLimit) || !has(self.memoryRequest) || quantity(self.memoryLimit).isGreaterThan(quantity(self.memoryRequest)) || quantity(self.memoryLimit).compareTo(quantity(self.memoryRequest)) == 0",message="memoryLimit must be at least memoryRequest"
type ExecutionSpec struct {
	// CPU request for the celld container (Kubernetes quantity). Default 250m.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?m?$`
	CPURequest string `json:"cpuRequest,omitempty"`
	// CPU limit for the celld container. Default: none.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?m?$`
	CPULimit string `json:"cpuLimit,omitempty"`
	// Memory request for the celld container. Default 512Mi.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	// Memory limit for the celld container. Default 1Gi.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$`
	MemoryLimit string `json:"memoryLimit,omitempty"`
	// Hard resident-cell admission limit (CELLD_MAX_RESIDENT_CELLS). Unset leaves
	// the runtime default.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	MaxResidentCells int32 `json:"maxResidentCells,omitempty"`
	// Seconds after which an idle resident cell hibernates (CELLD_IDLE_EVICT_S).
	// Unset leaves only pressure and the residency cap to evict idle cells.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	IdleEvictSeconds int32 `json:"idleEvictSeconds,omitempty"`
}

// LifecycleSpec bounds shutdown. The runtime's total stop budget must leave room
// inside the pod's termination grace for signal delivery and the launcher's
// lock proof; a longer budget is an opportunity to hand off, not proof of it.
// +kubebuilder:validation:XValidation:rule="!has(self.shutdownSeconds) || !has(self.terminationGraceSeconds) || self.shutdownSeconds + 5 <= self.terminationGraceSeconds",message="terminationGraceSeconds must exceed shutdownSeconds by at least 5"
// +kubebuilder:validation:XValidation:rule="!has(self.shutdownSeconds) || has(self.terminationGraceSeconds) || self.shutdownSeconds + 5 <= 30",message="shutdownSeconds above 25 requires an explicit terminationGraceSeconds"
// +kubebuilder:validation:XValidation:rule="has(self.shutdownSeconds) || !has(self.terminationGraceSeconds) || self.terminationGraceSeconds >= 25",message="terminationGraceSeconds must be at least 25 with the default 20 second shutdown"
type LifecycleSpec struct {
	// celld total stop bound in seconds (CELLD_SHUTDOWN_TOTAL_MS). Default 20.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	ShutdownSeconds int32 `json:"shutdownSeconds,omitempty"`
	// Pod terminationGracePeriodSeconds. Default 30. Must exceed shutdownSeconds by 5.
	// +optional
	// +kubebuilder:validation:Minimum=6
	// +kubebuilder:validation:Maximum=3605
	TerminationGraceSeconds int32 `json:"terminationGraceSeconds,omitempty"`
}

// Effective tuning with the prototype constants filled in for omitted fields.
// Existing fleets carry no execution or lifecycle block and keep exactly the
// template they were created with.
const (
	DefaultCPURequest              = "250m"
	DefaultMemoryRequest           = "512Mi"
	DefaultMemoryLimit             = "1Gi"
	DefaultShutdownSeconds   int32 = 20
	DefaultTerminationGrace  int32 = 30
	terminationGraceHeadroom int32 = 5
)

func (s *CelldFleetSpec) EffectiveExecution() ExecutionSpec {
	e := ExecutionSpec{CPURequest: DefaultCPURequest, MemoryRequest: DefaultMemoryRequest, MemoryLimit: DefaultMemoryLimit}
	if s.Execution == nil {
		return e
	}
	if s.Execution.CPURequest != "" {
		e.CPURequest = s.Execution.CPURequest
	}
	e.CPULimit = s.Execution.CPULimit
	if s.Execution.MemoryRequest != "" {
		e.MemoryRequest = s.Execution.MemoryRequest
	}
	if s.Execution.MemoryLimit != "" {
		e.MemoryLimit = s.Execution.MemoryLimit
	}
	e.MaxResidentCells = s.Execution.MaxResidentCells
	e.IdleEvictSeconds = s.Execution.IdleEvictSeconds
	return e
}

func (s *CelldFleetSpec) EffectiveLifecycle() LifecycleSpec {
	l := LifecycleSpec{ShutdownSeconds: DefaultShutdownSeconds, TerminationGraceSeconds: DefaultTerminationGrace}
	if s.Lifecycle == nil {
		return l
	}
	if s.Lifecycle.ShutdownSeconds != 0 {
		l.ShutdownSeconds = s.Lifecycle.ShutdownSeconds
	}
	if s.Lifecycle.TerminationGraceSeconds != 0 {
		l.TerminationGraceSeconds = s.Lifecycle.TerminationGraceSeconds
	}
	return l
}

// MaintenanceSpec requests suspension or a qualified planned restart.
type MaintenanceSpec struct {
	// Explicitly permit an operation that stops the entire fleet.
	AllowCoordinatedDowntime bool `json:"allowCoordinatedDowntime,omitempty"`
	// Pause new actions and unissued operations; continue recovery of issued actions.
	Paused bool `json:"paused,omitempty"`
	// Change to a new nonempty token to request a same-version restart. The current completed token is not replayed. Placement and verified shutdown prerequisites must pass.
	// +kubebuilder:validation:MaxLength=128
	RestartToken string `json:"restartToken,omitempty"`
}

// One entire bucket is reserved, including all runtime metadata and peer keys.
// Prefix multiplexing and alternate S3 authorities are deliberately unsupported.
type StorageSpec struct {
	// Name of the dedicated S3 bucket. The operator permanently reserves it for this fleet identity.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	Bucket string `json:"bucket"`
	// AWS region containing the bucket and the configured availability zones.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	Region string `json:"region"`
	// Required only for PersistentFleet; must reference an existing CSI Delete/WaitForFirstConsumer class. Disks are disposed only after strict shutdown proof; CSI deletion protection is required.
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
	// Number of availability zones; must equal the number of entries in zones.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=6
	AZCount int32 `json:"azCount"`
	// Strict requires zone balance and distinct hosts. Relaxed makes spread and host separation preferences while retaining the zone allowlist.
	// +kubebuilder:default=Strict
	// +kubebuilder:validation:Enum=Strict;Relaxed
	Mode string `json:"mode,omitempty"`
}

// LifecycleStatus is an informational projection of the bounded current operation.
// Clearing status never cancels an operation or removes recovery evidence.
type LifecycleStatus struct {
	// RFC3339 start time of the active operation, when available.
	StartedAt string `json:"startedAt,omitempty"`
	// RFC3339 completion time of the last completed operation, when available.
	LastCompletionAt string `json:"lastCompletionAt,omitempty"`
	// Outcome recorded in the last completed operation.
	LastOutcome string `json:"lastOutcome,omitempty"`

	// Reason the current operation cannot progress.
	Blocker string `json:"blocker,omitempty"`
	// RFC3339 operation deadline. Passing it reports a stall; it does not cancel recovery.
	Deadline string `json:"deadline,omitempty"`
	// Whether the active operation has exceeded its persisted deadline.
	Stalled bool `json:"stalled,omitempty"`
	// Kind of a current maintenance or runtime-transition request.
	RequestKind string `json:"requestKind,omitempty"`
	// Identifier or token of the current request.
	RequestID string `json:"requestID,omitempty"`
	// Requested runtime image of a current transition request.
	TargetImage string `json:"targetImage,omitempty"`
	// Identifier of the active capacity or maintenance operation.
	OperationID string `json:"operationID,omitempty"`
	// Current phase of the active operation; use the condition message for the next action.
	Phase string `json:"phase,omitempty"`
	// Starting replica count of the active capacity operation.
	From int32 `json:"from,omitempty"`
	// Target replica count of the active capacity operation.
	To int32 `json:"to,omitempty"`
	// Pod selected for the active capacity operation, when applicable.
	TargetPod string `json:"targetPod,omitempty"`
	// Exact Kubernetes UID of the selected target Pod.
	TargetUID string `json:"targetUID,omitempty"`
	// Runtime session generation of the selected target.
	TargetGeneration string `json:"targetGeneration,omitempty"`
}

type CelldFleetStatus struct {
	// Replicas and LabelSelector serve the /scale subresource: non-terminal pods
	// of this fleet, including terminating ones, and the selector that matches
	// exactly them. They never express permission to scale.
	// Zero is serialized so the scale view always carries a count, but the schema
	// does not require it: a status written before this field existed stays valid.
	// +optional
	Replicas int32 `json:"replicas"`
	// Serialized selector for exactly this fleet's Pods, for the /scale subresource.
	LabelSelector string `json:"labelSelector,omitempty"`
	// Current operation or enabled policy target, otherwise spec.replicas; zero during deletion.
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`
	// Replica target currently applied to the owned Kubernetes workload.
	AppliedReplicas int32 `json:"appliedReplicas,omitempty"`
	// Number of owned Pods observed in the latest complete inventory, including terminating Pods.
	ObservedReplicas int32 `json:"observedReplicas,omitempty"`
	// Observed non-terminating Pods that are not ready yet.
	JoiningReplicas int32 `json:"joiningReplicas,omitempty"`
	// Observed Pods with a deletion timestamp.
	TerminatingReplicas int32 `json:"terminatingReplicas,omitempty"`
	// Whether the Pod inventory was complete. False means counts must not be interpreted as proof of no running processes.
	ReplicaObservationValid bool `json:"replicaObservationValid,omitempty"`
	// RFC3339 time when the fleet entered its current continuous Blocked state; empty when not blocked.
	BlockedSince string `json:"blockedSince,omitempty"`
	// Latest capacity policy recommendation and observation coverage.
	Capacity CapacityStatus `json:"capacity,omitempty"`
	// Progress and current findings for capacity and maintenance operations.
	Lifecycle LifecycleStatus `json:"lifecycle,omitempty"`
	// Fleet metadata.generation reflected by this status update.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Ready count from the owned workload status, provided it covers the current workload generation.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Name of the cluster-scoped storage reservation holding this fleet's bucket ownership and current operation.
	Reservation string `json:"reservation,omitempty"`
	// Current readiness, progress, blocked, maintenance, and experimental-validation conditions.
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
	InitialReplicas int32 `json:"initialReplicas,omitempty"`
	// Dedicated S3 bucket permanently bound to this reservation.
	Bucket string `json:"bucket"`
	// Namespace of the fleet that owns the reservation.
	FleetNamespace string `json:"fleetNamespace"`
	// Name of the fleet that owns the reservation.
	FleetName string `json:"fleetName"`
	// Exact Kubernetes UID of the owning fleet; recreating a fleet with the same name does not transfer the reservation.
	FleetUID string `json:"fleetUID"`
	// Fingerprint of the original immutable fleet configuration used to detect conflicting reuse.
	SpecHash string `json:"specHash"`
}

// +kubebuilder:object:root=true
type CelldStorageReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldStorageReservation `json:"items"`
}
