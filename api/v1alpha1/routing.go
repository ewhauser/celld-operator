package v1alpha1

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// RoutingSpec exposes only the application Service on port 8080.
// +kubebuilder:validation:XValidation:rule="has(self.gateway) != has(self.ingress)",message="choose exactly one of gateway or ingress"
type RoutingSpec struct {
	// Explicit hostnames; no catch-all route is created.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	Hostnames []RouteHostname `json:"hostnames"`
	// Pods that originate application traffic, not the control-plane controller.
	Source RoutingSource `json:"source"`
	// Attach an HTTPRoute to an existing Gateway. TLS belongs to its listener.
	// +optional
	Gateway *GatewayRouting `json:"gateway,omitempty"`
	// Create a networking.k8s.io/v1 Ingress using an installed controller.
	// +optional
	Ingress *IngressRouting `json:"ingress,omitempty"`
}

// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
type RouteHostname string

type RoutingSource struct {
	// Exact namespace containing the gateway or ingress data-plane pods.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// Nonempty matchLabels selector in that namespace.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=16
	PodLabels map[string]string `json:"podLabels"`
}

type GatewayRouting struct {
	// Name of an existing Gateway.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// Gateway namespace. Omission uses the fleet namespace.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
	// Listener name. Omission attaches to compatible listeners.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SectionName string `json:"sectionName,omitempty"`
}

type IngressRouting struct {
	// Explicit installed IngressClass.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ClassName string `json:"className"`
	// TLS Secret in the fleet namespace, covering every configured hostname.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	TLSSecretName string `json:"tlsSecretName,omitempty"`
	// Controller-specific annotations, including optional cert-manager settings.
	// Treat permission to configure these as privileged ingress administration.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('celld.eric.dev/') && k != 'kubernetes.io/ingress.class')",message="operator annotations and the legacy ingress class annotation are reserved"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(bytes(self[k])) <= 4096)",message="annotation values must not exceed 4096 bytes"
	Annotations map[string]string `json:"annotations,omitempty"`
}

func validateRouting(r *RoutingSpec) error {
	if r == nil {
		return nil
	}
	if (r.Gateway == nil) == (r.Ingress == nil) {
		return fmt.Errorf("routing requires exactly one of gateway or ingress")
	}
	if len(r.Hostnames) < 1 || len(r.Hostnames) > 16 {
		return fmt.Errorf("routing requires 1..16 hostnames")
	}
	seen := map[RouteHostname]bool{}
	for _, h := range r.Hostnames {
		name := strings.TrimPrefix(string(h), "*.")
		if len(h) > 253 || net.ParseIP(name) != nil || len(validation.IsDNS1123Subdomain(name)) != 0 || seen[h] {
			return fmt.Errorf("routing hostnames must be unique DNS names, optionally using a wildcard prefix")
		}
		seen[h] = true
	}
	if len(validation.IsDNS1123Label(r.Source.Namespace)) != 0 || len(r.Source.PodLabels) < 1 || len(r.Source.PodLabels) > 16 {
		return fmt.Errorf("routing.source requires an explicit namespace and 1..16 pod labels")
	}
	for k, v := range r.Source.PodLabels {
		if len(validation.IsQualifiedName(k)) != 0 || len(validation.IsValidLabelValue(v)) != 0 {
			return fmt.Errorf("routing.source.podLabels contains an invalid label")
		}
	}
	if g := r.Gateway; g != nil {
		if len(validation.IsDNS1123Subdomain(g.Name)) != 0 || (g.Namespace != "" && len(validation.IsDNS1123Label(g.Namespace)) != 0) || (g.SectionName != "" && len(validation.IsDNS1123Subdomain(g.SectionName)) != 0) {
			return fmt.Errorf("routing.gateway contains an invalid reference")
		}
	}
	if i := r.Ingress; i != nil {
		if len(validation.IsDNS1123Subdomain(i.ClassName)) != 0 || (i.TLSSecretName != "" && len(validation.IsDNS1123Subdomain(i.TLSSecretName)) != 0) {
			return fmt.Errorf("routing.ingress requires a valid className and optional TLS secret name")
		}
		if len(i.Annotations) > 16 {
			return fmt.Errorf("routing.ingress supports at most 16 annotations")
		}
		for k, v := range i.Annotations {
			if len(validation.IsQualifiedName(k)) != 0 || strings.HasPrefix(k, "celld.eric.dev/") || k == "kubernetes.io/ingress.class" || len(v) > 4096 {
				return fmt.Errorf("routing.ingress contains an invalid or reserved annotation")
			}
		}
	}
	return nil
}
