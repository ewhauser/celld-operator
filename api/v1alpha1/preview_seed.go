package v1alpha1

import (
	"fmt"
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PreviewSeedSpec selects persisted objects, copied once before runtime startup.
// Snapshots are consistent per object, not globally across the selection.
type PreviewSeedSpec struct {
	// Administrator-approved alias from the pool's seeding.sources.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Source string `json:"source"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=class
	// +listMapKey=id
	Objects []PreviewObjectReference `json:"objects"`
	// Clear disables copied alarms. Preserve explicitly opts into scheduled work.
	// +kubebuilder:default=Clear
	// +kubebuilder:validation:Enum=Clear;Preserve
	Alarms string `json:"alarms,omitempty"`
}

type PreviewObjectReference struct {
	// Exported Durable Object class, with the same mapping in the target application.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_$.-]+$`
	Class string `json:"class"`
	// Canonical object ID, not an idFromName input or a storage path.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_$.-]+$`
	ID string `json:"id"`
}

type PreviewSeedingSpec struct {
	// Trusted executor implementation that understands the seed request protocol.
	// The operator does not install this executor or grant it source credentials.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Executor string `json:"executor"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Sources []PreviewSeedSource `json:"sources"`
}

type PreviewSeedSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name     string             `json:"name"`
	FleetRef SeedFleetReference `json:"fleetRef"`
}

type SeedFleetReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Exact authorized fleet identity; recreating a name never transfers access.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid"`
}

// SeedReference permanently binds a fleet to its initialization request.
type SeedReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid"`
}

// CelldPreviewSeed is a retained initialization request for a trusted external
// celld executor. It is never garbage-collected with the preview. Terminal status
// attests that all executor writes have stopped. No executor is bundled here.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cps
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Executor",type=string,JSONPath=`.spec.request.executor`
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || !(oldSelf.status.phase in ['Succeeded', 'Failed', 'Canceled']) || (has(self.status) && self.status == oldSelf.status)",message="terminal seed results cannot change"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.manifest) || (has(self.status) && has(self.status.manifest) && self.status.manifest == oldSelf.status.manifest)",message="captured snapshot manifest cannot change or be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.targetFleetUID) || (has(self.status) && has(self.status.targetFleetUID) && self.status.targetFleetUID == oldSelf.status.targetFleetUID)",message="executor target identity cannot change"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Running' || ((!has(self.spec.canceled) || !self.spec.canceled) && (!has(oldSelf.status) || !has(oldSelf.status.phase) || oldSelf.status.phase in ['Pending', 'Running'])) || (has(oldSelf.status) && has(oldSelf.status.phase) && oldSelf.status.phase == 'Running')",message="canceled seed requests cannot be claimed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Succeeded' || ((!has(self.spec.canceled) || !self.spec.canceled) && has(self.status.manifest) && has(self.status.targetFleetUID))",message="success requires an uncanceled request, target identity and manifest"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || !(self.status.phase in ['Running', 'Succeeded']) || (has(self.status.executorID) && has(self.status.targetFleetUID))",message="executor claim requires execution and target identities"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.executorID) || (has(self.status) && has(self.status.executorID) && self.status.executorID == oldSelf.status.executorID)",message="executor identity cannot change"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Succeeded' || (has(oldSelf.status) && has(oldSelf.status.phase) && oldSelf.status.phase in ['Running', 'Succeeded'])",message="success requires a previously claimed execution"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || oldSelf.status.phase != 'Running' || (has(self.status) && has(self.status.phase) && self.status.phase != 'Pending')",message="claimed requests cannot return to pending"
type CelldPreviewSeed struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CelldPreviewSeedSpec   `json:"spec"`
	Status            CelldPreviewSeedStatus `json:"status,omitempty"`
}

type CelldPreviewSeedSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="seed request is immutable"
	Request PreviewSeedRequest `json:"request"`
	// Cancellation is monotonic. Running executors must stop writes before acknowledging it.
	// +kubebuilder:default=false
	// +kubebuilder:validation:XValidation:rule="!oldSelf || self",message="seed cancellation cannot be reversed"
	Canceled bool `json:"canceled,omitempty"`
}

type PreviewSeedRequest struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Executor    string             `json:"executor"`
	SourceFleet SeedFleetReference `json:"sourceFleet"`
	Target      PreviewSeedTarget  `json:"target"`
	Selection   PreviewSeedSpec    `json:"selection"`
	// Executor must stop before this time and must never begin after it.
	Deadline metav1.Time `json:"deadline"`
}

type PreviewSeedTarget struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PreviewName string `json:"previewName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	PreviewUID string `json:"previewUID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	FleetName string               `json:"fleetName"`
	PoolRef   StoragePoolReference `json:"poolRef"`
	// Exact isolated S3 destination; no credentials.
	// +kubebuilder:validation:MaxLength=133
	// +kubebuilder:validation:Pattern=`^s3://[a-z0-9-]+/[a-z0-9-]+$`
	StorageURL string `json:"storageURL"`
}

type CelldPreviewSeedStatus struct {
	// Stable execution identity. Other workers cannot take over a Running request.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ExecutorID string `json:"executorID,omitempty"`
	// Running is an exclusive claim, published with resourceVersion before any I/O.
	// Terminal phases certify there are no active or retryable writes left.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Canceled
	Phase string `json:"phase,omitempty"`
	// Exact destination fleet UID whose prefix reservation was verified by the executor.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	TargetFleetUID string `json:"targetFleetUID,omitempty"`
	// Publish all snapshot identities atomically before importing any objects.
	// The manifest is immutable and retained for repeatable retries and audit.
	Manifest *PreviewSnapshotManifest `json:"manifest,omitempty"`
	// Bounded, non-sensitive progress or failure summary; never secret values.
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

type PreviewSnapshotManifest struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=class
	// +listMapKey=id
	Objects []PreviewObjectSnapshot `json:"objects"`
}

type PreviewObjectSnapshot struct {
	PreviewObjectReference `json:",inline"`
	// Opaque immutable snapshot handle understood by the executor, never a live source pointer.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	SnapshotID string `json:"snapshotID"`
	// Committed source version captured for this object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	SourceVersion string `json:"sourceVersion"`
	// SHA-256 of the exported snapshot payload.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	Digest string `json:"digest"`
}

// +kubebuilder:object:root=true
type CelldPreviewSeedList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldPreviewSeed `json:"items"`
}

func (s *PreviewSeedSpec) Validate() error {
	if s == nil {
		return nil
	}
	if len(validation.IsDNS1123Label(s.Source)) != 0 || len(s.Objects) < 1 || len(s.Objects) > 100 || (s.Alarms != "" && s.Alarms != "Clear" && s.Alarms != "Preserve") {
		return fmt.Errorf("seed requires an approved source alias, 1-100 objects and Clear or Preserve alarms")
	}
	seen := map[PreviewObjectReference]bool{}
	valid := regexp.MustCompile(`^[A-Za-z0-9_$.-]+$`)
	for _, o := range s.Objects {
		if len(o.Class) > 128 || len(o.ID) > 256 || !valid.MatchString(o.Class) || !valid.MatchString(o.ID) || seen[o] {
			return fmt.Errorf("seed objects require unique valid class and canonical ID pairs")
		}
		seen[o] = true
	}
	return nil
}
