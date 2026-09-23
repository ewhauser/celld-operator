package v1alpha1

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// CelldPreviewPool is administrator-owned configuration shared by previews in
// one namespace. Each preview has its own runtime and single-segment S3 prefix.
// The bucket is permanently reserved to this pool UID on first use.
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=cpp
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.spec.storage.bucket`
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.routing.baseDomain`
type CelldPreviewPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Create a new pool for configuration changes; existing preview identities never move.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="preview pool configuration is immutable"
	Spec CelldPreviewPoolSpec `json:"spec"`
}

// +kubebuilder:validation:XValidation:rule="self.zone.startsWith(self.storage.region) && size(self.zone) == size(self.storage.region) + 1 && self.zone.matches('.*[a-z]$')",message="zone must belong to the storage region"
type CelldPreviewPoolSpec struct {
	// Opt-in source authorization and trusted executor for state seeding.
	// +optional
	Seeding *PreviewSeedingSpec `json:"seeding,omitempty"`
	// Immutable celld runtime digest, shared by the independent preview runtimes.
	// +kubebuilder:validation:Pattern=`^([a-z0-9]+([.-][a-z0-9]+)*|localhost)(:[0-9]{1,5})?(/[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*)+@sha256:[a-f0-9]{64}$`
	RuntimeImage string `json:"runtimeImage"`
	// Existing ServiceAccount in the pool/preview namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ServiceAccountName string             `json:"serviceAccountName"`
	Storage            PreviewPoolStorage `json:"storage"`
	// One allowed zone. Previews use a single replica and relaxed host placement.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=33
	Zone    string         `json:"zone"`
	Routing PreviewRouting `json:"routing"`
	// Omitted values use preview defaults: 25m CPU, 64Mi request, 256Mi limit,
	// eight resident cells and 30-second idle eviction.
	// +optional
	Execution *ExecutionSpec `json:"execution,omitempty"`
	// Disk-backed scratch; defaults to a 64Mi request and 512Mi limit.
	// +optional
	Scratch *ScratchSpec `json:"scratch,omitempty"`
}

type PreviewPoolStorage struct {
	// Existing bucket dedicated to this pool; the operator does not create or delete it.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	Region string `json:"region"`
	// Omit for AWS S3 with ServiceAccount credentials. Set for a shared local store.
	// +optional
	Endpoint *ObjectStoreEndpoint `json:"endpoint,omitempty"`
}

type ObjectStoreEndpoint struct {
	// HTTP(S) origin only. HTTP explicitly opts into plaintext transport.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https?://[^/?#@]+/?$`
	URL string `json:"url"`
	// Same-namespace Secret with accessKeyId and secretAccessKey keys.
	// Values are injected by kubelet, never read into controller status.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	CredentialsSecretName string `json:"credentialsSecretName"`
	// Narrow object-store destination, independent of fleet peer ingress.
	Egress CollectorEgress `json:"egress"`
}

// +kubebuilder:validation:XValidation:rule="quantity(self.limit).compareTo(quantity(self.request)) >= 0",message="scratch limit must cover request"
// +kubebuilder:validation:XValidation:rule="quantity(self.request).compareTo(quantity('0')) > 0",message="scratch request must be positive"
type ScratchSpec struct {
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$`
	Request string `json:"request"`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$`
	Limit string `json:"limit"`
}

type PreviewPoolReference struct {
	// Pool in the same namespace as the preview.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

type StoragePoolReference struct {
	// Pool in the same namespace as the fleet.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
	// Exact pool UID; names alone cannot transfer a reservation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	UID string `json:"uid"`
}

// +kubebuilder:object:root=true
type CelldPreviewPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldPreviewPool `json:"items"`
}

// FleetSpec supplies the small independent runtime behind one preview. The
// prefix is a single path segment so different reservations cannot overlap.
func (p *CelldPreviewPool) FleetSpec(prefix string) CelldFleetSpec {
	e := ExecutionSpec{}
	if p.Spec.Execution != nil {
		e = *p.Spec.Execution
	}
	if e.CPURequest == "" {
		e.CPURequest = "25m"
	}
	if e.MemoryRequest == "" {
		e.MemoryRequest = "64Mi"
	}
	if e.MemoryLimit == "" {
		e.MemoryLimit = "256Mi"
	}
	if e.MaxResidentCells == 0 {
		e.MaxResidentCells = 8
	}
	if e.IdleEvictSeconds == 0 {
		e.IdleEvictSeconds = 30
	}
	scratch := &ScratchSpec{Request: "64Mi", Limit: "512Mi"}
	if p.Spec.Scratch != nil {
		scratch = p.Spec.Scratch.DeepCopy()
	}
	// Keep preserved cache within a quarter of scratch; leave the rest for live
	// SQLite files, journals and launch metadata. Invalid quantities are rejected
	// by fleet validation before rendering any workload.
	cacheBytes := int64(128 * 1024 * 1024)
	if limit, err := resource.ParseQuantity(scratch.Limit); err == nil {
		cacheBytes = max(1, min(cacheBytes, limit.Value()/4))
	}
	values := [][2]string{{"CELLD_MAX_STATELESS_ISOLATES", "1"}, {"CELLD_MAX_REQUESTS", "8"}, {"CELLD_MAX_CELL_REQUESTS", "4"}, {"CELLD_MAX_REQUEST_BODY_BYTES", "1048576"}, {"CELLD_LOCAL_CACHE_MAX_BYTES", strconv.FormatInt(cacheBytes, 10)}}
	env := make([]FleetEnvVar, 0, len(values))
	for _, pair := range values {
		env = append(env, FleetEnvVar{Name: pair[0], Value: new(pair[1])})
	}
	return CelldFleetSpec{RuntimeImage: p.Spec.RuntimeImage, Profile: "Bucket", BucketWorkload: "Ordered", Replicas: 1,
		ServiceAccountName: p.Spec.ServiceAccountName,
		Storage: StorageSpec{Bucket: p.Spec.Storage.Bucket, Region: p.Spec.Storage.Region, SizeGiB: 10,
			Prefix: prefix, PoolRef: &StoragePoolReference{Name: p.Name, UID: string(p.UID)}, Endpoint: p.Spec.Storage.Endpoint.DeepCopy(), Scratch: scratch},
		Execution: &e, Env: env, Placement: PlacementSpec{AZCount: 1, Zones: []string{p.Spec.Zone}, Mode: "Relaxed"}}
}

func validateStorageOptions(s StorageSpec, profile string) error {
	if (s.Prefix == "") != (s.PoolRef == nil) {
		return fmt.Errorf("storage prefix and poolRef must be supplied together")
	}
	if s.Prefix != "" && (profile != "Bucket" || len(validation.IsDNS1123Label(s.Prefix)) != 0 || len(s.Prefix) > 63) {
		return fmt.Errorf("shared storage requires Bucket profile and one DNS-label prefix")
	}
	if s.PoolRef != nil && (len(validation.IsDNS1123Subdomain(s.PoolRef.Name)) != 0 || s.PoolRef.UID == "" || len(s.PoolRef.UID) > 64) {
		return fmt.Errorf("invalid storage pool identity")
	}
	if s.Initialization != nil && (s.PoolRef == nil || len(validation.IsDNS1123Label(s.Initialization.Name)) != 0 || s.Initialization.UID == "" || len(s.Initialization.UID) > 64) {
		return fmt.Errorf("initialization requires a shared pool and exact seed request identity")
	}
	if s.Endpoint != nil {
		if s.PoolRef == nil {
			return fmt.Errorf("custom storage endpoints require a shared pool reservation")
		}
		u, err := url.Parse(s.Endpoint.URL)
		if err != nil || u == nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(s.Endpoint.URL, "\\\r\n\t") {
			return fmt.Errorf("storage endpoint must be an HTTP(S) origin")
		}
		if err := validateTelemetry(&TelemetrySpec{CollectorURL: s.Endpoint.URL, Egress: s.Endpoint.Egress}); err != nil {
			return fmt.Errorf("storage endpoint: %w", err)
		}
		if len(validation.IsDNS1123Subdomain(s.Endpoint.CredentialsSecretName)) != 0 {
			return fmt.Errorf("invalid storage credentials Secret name")
		}
	}
	if s.Scratch != nil {
		if profile != "Bucket" {
			return fmt.Errorf("scratch overrides require Bucket profile")
		}
		request, e1 := resource.ParseQuantity(s.Scratch.Request)
		limit, e2 := resource.ParseQuantity(s.Scratch.Limit)
		if e1 != nil || e2 != nil || request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			return fmt.Errorf("scratch requires positive quantities with request <= limit")
		}
	}
	return nil
}

func (s StorageSpec) URL() string {
	result := "s3://" + s.Bucket
	if s.Prefix != "" {
		result += "/" + s.Prefix
	}
	return result
}
