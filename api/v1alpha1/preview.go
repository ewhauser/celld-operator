package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CelldPreview provisions an isolated application fleet with a bounded lifetime.
// Application code is uploaded separately to its isolated prefix in the pool bucket.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cp
// +kubebuilder:printcolumn:name="Fleet",type=string,JSONPath=`.status.fleetName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
type CelldPreview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CelldPreviewSpec   `json:"spec"`
	Status            CelldPreviewStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.seed) == has(oldSelf.seed) && (!has(self.seed) || self.seed == oldSelf.seed)",message="preview seed selection is fixed at creation"
type CelldPreviewSpec struct {
	// Optional one-time multi-object initialization; omission starts empty.
	// +optional
	Seed *PreviewSeedSpec `json:"seed,omitempty"`
	// Shared platform configuration in this namespace; immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="preview pool reference is immutable"
	PoolRef PreviewPoolReference `json:"poolRef"`
	// Human-readable branch or pull request identifier. Never used as a resource name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Source string `json:"source"`
	// Optional informational revision supplied by CI, not deployment verification.
	// +kubebuilder:validation:MaxLength=256
	Revision string `json:"revision,omitempty"`
	// Lifetime from creation; immutable. Expiry initiates safe fleet deletion.
	// +kubebuilder:default=86400
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=604800
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ttlSeconds is immutable"
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
}

type CelldPreviewStatus struct {
	// Initialization request, retained separately from the preview.
	SeedName string `json:"seedName,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Canceled
	SeedPhase          string `json:"seedPhase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	FleetName          string `json:"fleetName,omitempty"`
	PoolName           string `json:"poolName,omitempty"`
	PoolUID            string `json:"poolUID,omitempty"`
	// Target for celld deploy; contains no credentials.
	StorageURL string `json:"storageURL,omitempty"`
	// Internal HTTP endpoint; Ready confirms infrastructure, not application deployment.
	Endpoint string `json:"endpoint,omitempty"`
	// Unique URL; configure wildcard DNS and TLS on the selected edge controller.
	URL       string       `json:"url,omitempty"`
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Blocked;Deleting;Expired
	Phase string `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type CelldPreviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CelldPreview `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="has(self.gateway) != has(self.ingress)",message="choose exactly one of gateway or ingress"
// +kubebuilder:validation:XValidation:rule="self.scheme != 'https' || !has(self.ingress) || has(self.ingress.tlsSecretName)",message="HTTPS ingress requires a TLS secret"
type PreviewRouting struct {
	// Wildcard DNS for this domain must point to the Gateway or Ingress controller.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=218
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	BaseDomain string `json:"baseDomain"`
	// HTTPS requires a TLS listener on a Gateway or a wildcard TLS Secret for Ingress.
	// +kubebuilder:default=https
	// +kubebuilder:validation:Enum=http;https
	Scheme  string          `json:"scheme,omitempty"`
	Source  RoutingSource   `json:"source"`
	Gateway *GatewayRouting `json:"gateway,omitempty"`
	Ingress *IngressRouting `json:"ingress,omitempty"`
}
