// Package v1alpha1 contains the fleet API under the celld.eric.dev group.
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
	s.AddKnownTypes(GroupVersion, &CelldFleet{}, &CelldFleetList{}, &CelldPreview{}, &CelldPreviewList{}, &CelldStorageReservation{}, &CelldStorageReservationList{})
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
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.previews) || (has(self.previews) && self.previews == oldSelf.previews)",message="preview configuration cannot change once enabled"
	// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile && self.serviceAccountName == oldSelf.serviceAccountName && self.storage == oldSelf.storage && self.placement == oldSelf.placement && has(self.execution) == has(oldSelf.execution) && (!has(self.execution) || self.execution == oldSelf.execution) && has(self.lifecycle) == has(oldSelf.lifecycle) && (!has(self.lifecycle) || self.lifecycle == oldSelf.lifecycle) && has(self.env) == has(oldSelf.env) && (!has(self.env) || self.env == oldSelf.env) && has(self.telemetry) == has(oldSelf.telemetry) && (!has(self.telemetry) || self.telemetry == oldSelf.telemetry) && self.bucketWorkload == oldSelf.bucketWorkload",message="runtime storage, layout, placement, execution, lifecycle, env and telemetry are fixed at creation"
	Spec   CelldFleetSpec   `json:"spec"`
	Status CelldFleetStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.previews) || self.previews.storage.bucket != self.storage.bucket",message="preview storage must use a separate bucket from the parent runtime"
// +kubebuilder:validation:XValidation:rule="self.profile == 'Bucket' || (!has(self.storage.prefix) && !has(self.storage.scratch))",message="shared prefixes and scratch overrides require Bucket profile"
// +kubebuilder:validation:XValidation:rule="self.bucketWorkload != 'Ordered' || self.profile == 'Bucket'",message="Ordered bucketWorkload requires Bucket profile"
// +kubebuilder:validation:XValidation:rule="self.placement.azCount == size(self.placement.zones)",message="azCount must equal the zones count"
// +kubebuilder:validation:XValidation:rule="self.replicas >= self.placement.azCount",message="replicas must be at least azCount"
// +kubebuilder:validation:XValidation:rule="self.profile == 'PersistentFleet' ? has(self.storage.storageClassName) && size(self.storage.storageClassName) > 0 : !has(self.storage.storageClassName)",message="storageClassName is required only for PersistentFleet"
// +kubebuilder:validation:XValidation:rule="self.placement.zones.all(z, z.startsWith(self.storage.region) && size(z) == size(self.storage.region) + 1 && z.matches('.*[a-z]$'))",message="zones must be standard AZ names in storage.region"
// +kubebuilder:validation:XValidation:rule="!has(self.capacity) || self.capacity.minReplicas >= self.placement.azCount",message="capacity minimum must cover requested AZs"
type CelldFleetSpec struct {
	// Optional immutable configuration for previews referencing this fleet.
	// +optional
	Previews *FleetPreviewsSpec `json:"previews,omitempty"`
	// Bucket workload layout. Defaults to Deployment; Ordered uses deterministic ordinal removal. Layout is immutable.
	// +kubebuilder:default=Deployment
	// +kubebuilder:validation:Enum=Deployment;Ordered
	BucketWorkload string `json:"bucketWorkload,omitempty"`
	// Requested immutable runtime digest. Required for provisioning; no default release is assumed.
	// Use a homogeneous compatible fork; verify release and recovery compatibility.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-z0-9]+([.-][a-z0-9]+)*|localhost)(:[0-9]{1,5})?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`
	RuntimeImage string `json:"runtimeImage,omitempty"`
	// Pause workload changes or request a rolling same-version restart.
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`

	// Storage profile: Bucket uses temporary local disk and S3; PersistentFleet gives each member a persistent disk it keeps until the fleet is deleted. Immutable after creation.
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
	// Reserved object-store scope and local disk settings. Immutable after creation.
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
	// Additional celld settings, fixed at creation. Secret values stay in the
	// referenced Secret; changes to Secret data take effect only in new Pods.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(e, !(['CELLD_NODE','CELLD_ADVERTISE','CELLD_BUCKET','CELLD_DURABILITY','CELLD_ADDR','CELLD_INTERNAL_ADDR','CELLD_WATCH','CELLD_TTL_MS','CELLD_SHUTDOWN_TOTAL_MS','CELLD_TOKIO_THREADS','CELLD_MAX_RESIDENT_CELLS','CELLD_IDLE_EVICT_S'].exists(n, n == e.name) || e.name.startsWith('CELLD_REEXEC_') || e.name.startsWith('CELLD_OTEL') || e.name.startsWith('CELLD_UNSAFE_') || e.name.startsWith('CELLD_TEST_') || e.name.startsWith('CELLD_STRICT_')))",message="env may not override operator-owned or reserved celld variables"
	Env []FleetEnvVar `json:"env,omitempty"`
	// Optional OTLP collector; omission leaves telemetry disabled. Immutable
	// because the operator does not roll out ordinary pod-template changes.
	// +optional
	Telemetry *TelemetrySpec `json:"telemetry,omitempty"`
	// Optional public HTTP routing. Mutable without restarting the runtime.
	// Omission removes operator-owned routes and their separate ingress policy.
	// +optional
	Routing *RoutingSpec `json:"routing,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.secretKeyRef)",message="exactly one of value or secretKeyRef is required"
type FleetEnvVar struct {
	// +kubebuilder:validation:Pattern=`^CELLD_[A-Z][A-Z0-9_]*$`
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`
	// Literal, including an empty string. Use secretKeyRef for credentials.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Value *string `json:"value,omitempty"`
	// Secret in the same namespace as the fleet.
	// +optional
	SecretKeyRef *SecretKeyRef `json:"secretKeyRef,omitempty"`
}

type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// TelemetrySpec maps to celld's current OTLP environment contract. The
// collector destination is explicitly permitted by a narrow NetworkPolicy rule.
// +kubebuilder:validation:XValidation:rule="has(self.sampler) && (self.sampler == 'traceidratio' || self.sampler == 'parentbased_traceidratio') ? has(self.samplerArg) : !has(self.samplerArg)",message="samplerArg is required only for ratio samplers"
type TelemetrySpec struct {
	// HTTP(S) collector base URL; celld appends /v1/traces and /v1/logs.
	// +kubebuilder:validation:Pattern=`^https?://[^/?#@]+(/[^?#]*)?$`
	CollectorURL string          `json:"collectorURL"`
	Egress       CollectorEgress `json:"egress"`
	// +optional
	// +kubebuilder:validation:Enum=always_on;always_off;parentbased_always_on;parentbased_always_off;traceidratio;parentbased_traceidratio
	Sampler string `json:"sampler,omitempty"`
	// +optional
	// Decimal ratio in [0,1], serialized as a string for portable CRD clients.
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	SamplerArg string `json:"samplerArg,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	FlushMilliseconds int64 `json:"flushMilliseconds,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	FlushBytes int64 `json:"flushBytes,omitempty"`
	// Secret value contains comma-separated OTLP headers; it never enters status.
	// +optional
	HeadersSecretKeyRef *TelemetrySecretKeyRef `json:"headersSecretKeyRef,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.cidr) != has(self.podLabels)",message="exactly one of cidr or podLabels is required"
type CollectorEgress struct {
	// Single collector address, /32 for IPv4 or /128 for IPv6.
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// Labels on collector pods. A namespace is optional for same-namespace pods.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	PodLabels map[string]string `json:"podLabels,omitempty"`
	// Collector namespace; only valid with podLabels.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

type TelemetrySecretKeyRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// ExecutionSpec sizes the celld container and bounds its residency. Values are
// starting points, not capacity guarantees; the capacity policy's thresholds are
// absolute and independent of these requests.
// +kubebuilder:validation:XValidation:rule="!has(self.cpuLimit) || !has(self.cpuRequest) || quantity(self.cpuLimit).isGreaterThan(quantity(self.cpuRequest)) || quantity(self.cpuLimit).compareTo(quantity(self.cpuRequest)) == 0",message="cpuLimit must be at least cpuRequest"
// +kubebuilder:validation:XValidation:rule="!has(self.memoryLimit) || !has(self.memoryRequest) || quantity(self.memoryLimit).isGreaterThan(quantity(self.memoryRequest)) || quantity(self.memoryLimit).compareTo(quantity(self.memoryRequest)) == 0",message="memoryLimit must be at least memoryRequest"
type ExecutionSpec struct {
	// CPU request for the celld container (Kubernetes quantity). Default 250m for fleets, 25m for preview pools.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?m?$`
	CPURequest string `json:"cpuRequest,omitempty"`
	// CPU limit for the celld container. Default: none.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?m?$`
	CPULimit string `json:"cpuLimit,omitempty"`
	// Memory request for the celld container. Default 512Mi for fleets, 64Mi for preview pools.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	// Memory limit for the celld container. Default 1Gi for fleets, 256Mi for preview pools.
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
// inside the pod's termination grace for signal delivery; a longer budget is an
// opportunity to hand off, not proof of it.
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

// MaintenanceSpec suspends workload changes or requests a rolling restart.
type MaintenanceSpec struct {
	// Deprecated and ignored: restarts and upgrades replace one member at a
	// time and need no downtime permission (ADR 0023).
	AllowCoordinatedDowntime bool `json:"allowCoordinatedDowntime,omitempty"`
	// Stop applying workload changes. A rollout the workload controller has already started continues.
	Paused bool `json:"paused,omitempty"`
	// Change to a new nonempty token to request a rolling same-version restart. The token is written to the Pod template, so a completed restart is not replayed.
	// +kubebuilder:validation:MaxLength=128
	RestartToken string `json:"restartToken,omitempty"`
}

// One entire bucket is reserved, including all runtime metadata and peer keys.
// Shared pools reserve the bucket before fleets claim disjoint single-segment prefixes.
// +kubebuilder:validation:XValidation:rule="has(self.prefix) == has(self.previewFleetRef)",message="prefix and previewFleetRef must be supplied together"
// +kubebuilder:validation:XValidation:rule="!has(self.initialization) || has(self.previewFleetRef)",message="initialization requires a pool reservation"
// +kubebuilder:validation:XValidation:rule="!has(self.endpoint) || has(self.previewFleetRef)",message="custom endpoints require a pool reservation"
type StorageSpec struct {
	// One-time startup gate, bound to an operator-owned seed request.
	// +optional
	Initialization *PreviewSeedRequest `json:"initialization,omitempty"`
	// Single segment within a pool-owned bucket. Nested/overlapping prefixes are forbidden.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Prefix string `json:"prefix,omitempty"`
	// Pool identity, permanently bound by the whole-bucket reservation.
	// +optional
	PreviewFleetRef *StorageFleetReference `json:"previewFleetRef,omitempty"`
	// +optional
	Endpoint *ObjectStoreEndpoint `json:"endpoint,omitempty"`
	// Disk-backed emptyDir request and limit; overrides sizeGiB only for Bucket.
	// +optional
	Scratch *ScratchSpec `json:"scratch,omitempty"`
	// S3 bucket permanently reserved to this fleet, or to the referenced preview pool.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	Bucket string `json:"bucket"`
	// AWS region containing the bucket and the configured availability zones.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	Region string `json:"region"`
	// Required only for PersistentFleet; must reference an existing CSI Delete/WaitForFirstConsumer class. Each member keeps its disk across restart, upgrade and scale-in; disks are deleted with the fleet.
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

// LifecycleStatus is retained for compatibility with earlier releases. The
// operator runs no lifecycle operations of its own and leaves it empty.
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
	// Read-only application deployment observations, independent of lifecycle readiness.
	// +optional
	Application *ApplicationStatus `json:"application,omitempty"`
	// Replicas and LabelSelector serve the /scale subresource: non-terminal pods
	// of this fleet, including terminating ones, and the selector that matches
	// exactly them. They never express permission to scale.
	// Zero is serialized so the scale view always carries a count, but the schema
	// does not require it: a status written before this field existed stays valid.
	// +optional
	Replicas int32 `json:"replicas"`
	// Serialized selector for exactly this fleet's Pods, for the /scale subresource.
	LabelSelector string `json:"labelSelector,omitempty"`
	// Enabled capacity-policy target, otherwise spec.replicas.
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
	// Retained for compatibility with earlier releases; always empty.
	Lifecycle LifecycleStatus `json:"lifecycle,omitempty"`
	// Fleet metadata.generation reflected by this status update.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Ready count from the owned workload status, provided it covers the current workload generation.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Name of the cluster-scoped storage reservation holding this fleet's bucket ownership.
	Reservation string `json:"reservation,omitempty"`
	// Current readiness, progress, blocked, and maintenance conditions.
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

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || !(oldSelf.status.phase in ['Succeeded', 'Failed', 'Canceled']) || (has(self.status) && self.status == oldSelf.status)",message="terminal seed results cannot change"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.manifest) || (has(self.status) && has(self.status.manifest) && self.status.manifest == oldSelf.status.manifest)",message="captured snapshot manifest cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.targetFleetUID) || (has(self.status) && has(self.status.targetFleetUID) && self.status.targetFleetUID == oldSelf.status.targetFleetUID)",message="executor target identity cannot change"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Running' || ((!has(self.status.canceled) || !self.status.canceled) && (!has(oldSelf.status) || !has(oldSelf.status.phase) || oldSelf.status.phase in ['Pending', 'Running'])) || (has(oldSelf.status) && has(oldSelf.status.phase) && oldSelf.status.phase == 'Running')",message="canceled seed requests cannot be claimed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Succeeded' || ((!has(self.status.canceled) || !self.status.canceled) && has(self.status.manifest) && has(self.status.targetFleetUID))",message="success requires an uncanceled request, target identity and manifest"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || !(self.status.phase in ['Running', 'Succeeded']) || (has(self.status.executorID) && has(self.status.targetFleetUID))",message="executor claim requires execution and target identities"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.executorID) || (has(self.status) && has(self.status.executorID) && self.status.executorID == oldSelf.status.executorID)",message="executor identity cannot change"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Succeeded' || (has(oldSelf.status) && has(oldSelf.status.phase) && oldSelf.status.phase in ['Running', 'Succeeded'])",message="success requires a previously claimed execution"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || oldSelf.status.phase != 'Running' || (has(self.status) && has(self.status.phase) && self.status.phase != 'Pending')",message="claimed requests cannot return to pending"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.canceled) || !oldSelf.status.canceled || (has(self.status) && has(self.status.canceled) && self.status.canceled)",message="seed cancellation cannot be removed or reversed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.targetFleetUID) || self.status.targetFleetUID == self.spec.fleetUID",message="executor must claim this reservation's fleet UID"
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || has(self.spec.initialization)",message="seed status requires an initialization request"
// CelldStorageReservation is a durable, cluster-wide tombstone. Never garbage collected.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type CelldStorageReservation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storage reservations cannot be transferred or changed"
	Spec   ReservationSpec   `json:"spec"`
	Status PreviewSeedStatus `json:"status,omitempty"`
}
type ReservationSpec struct {
	// Immutable seed request for this exclusive preview destination.
	// +optional
	Initialization *PreviewSeedRequest `json:"initialization,omitempty"`
	// Empty means CelldFleet for existing reservations. Pool reservations own the whole bucket.
	// +optional
	// +kubebuilder:validation:Enum=FleetPreviews
	OwnerKind string `json:"ownerKind,omitempty"`
	// Empty reserves the whole bucket. Nonempty reserves exactly one path segment.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// Custom object-store origin; bucket names remain globally reserved in this cluster.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// InitialReplicas binds the replica component of SpecHash before provisioning.
	// Optional only for reservations created before this field existed.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	InitialReplicas int32 `json:"initialReplicas,omitempty"`
	// S3 bucket permanently bound to this reservation's owner (and optional prefix).
	Bucket string `json:"bucket"`
	// Namespace of the fleet or pool that owns the reservation.
	FleetNamespace string `json:"fleetNamespace"`
	// Name of the fleet or pool that owns the reservation.
	FleetName string `json:"fleetName"`
	// Exact Kubernetes UID of the owning fleet or pool; reusing a name never transfers a reservation.
	FleetUID string `json:"fleetUID"`
	// Fingerprint of the original immutable owner configuration used to detect conflicting reuse.
	SpecHash string `json:"specHash"`
}

// +kubebuilder:object:root=true
type CelldStorageReservationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldStorageReservation `json:"items"`
}
