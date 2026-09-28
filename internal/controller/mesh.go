package controller

import (
	"context"
	"errors"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Istio sidecar mode (spec.mesh.istio). Under STRICT mTLS every caller of a
// member needs a mesh identity, and once any ALLOW AuthorizationPolicy selects
// the members, Istio denies whatever no ALLOW rule names. The operator
// therefore renders the sidecar, lets it reach istiod, and owns the one policy
// that keeps peer RPC and its own /state reads working: port 8081 accepts only
// the fleet's ServiceAccount and the operator's.

var authorizationPolicyGVK = schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "AuthorizationPolicy"}

const (
	istioInjectLabel = "sidecar.istio.io/inject"
	// FleetNameLabel names the fleet on meshed members, so your own
	// AuthorizationPolicies can select them without the fleet UID. The
	// workload selector stays FleetLabel.
	FleetNameLabel = "celld.eric.dev/fleet"
	// A native sidecar starts before celld and stops after it, so peer RPC
	// works through startup recovery and the SIGTERM handoff.
	istioNativeSidecar = "sidecar.istio.io/nativeSidecar"
	// The EKS Pod Identity agent is link-local plain HTTP; it bypasses the proxy
	// so credentials keep working under a REGISTRY_ONLY outbound policy.
	istioExcludeOutbound = "traffic.sidecar.istio.io/excludeOutboundIPRanges"
	podIdentityAgentCIDR = "169.254.170.23/32"
	// istiod serves sidecar configuration (xDS) and certificates on this port.
	istiodXDSPort = 15012
	// DefaultOperatorServiceAccount is the operator's ServiceAccount in
	// config/manager/operator.yaml.
	DefaultOperatorServiceAccount = "celld-operator"
)

func meshTemplate(f *fleet.CelldFleet, template *corev1.PodTemplateSpec) {
	if f.Spec.Mesh == nil {
		return
	}
	template.Labels[istioInjectLabel] = "true"
	template.Labels[FleetNameLabel] = f.Name
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[istioNativeSidecar] = "true"
	template.Annotations[istioExcludeOutbound] = podIdentityAgentCIDR
}

func meshEgress(f *fleet.CelldFleet) []networkingv1.NetworkPolicyEgressRule {
	if f.Spec.Mesh == nil {
		return nil
	}
	return []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": f.Spec.Mesh.Istio.ControlPlaneNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "istiod"}},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(istiodXDSPort))}},
	}}
}

// principal matches a ServiceAccount's SPIFFE identity in any trust domain.
func principal(namespace, serviceAccount string) string {
	return "*/ns/" + namespace + "/sa/" + serviceAccount
}

func desiredAuthorizationPolicy(f *fleet.CelldFleet, opts Options) *unstructured.Unstructured {
	operatorSA := opts.OperatorServiceAccount
	if operatorSA == "" {
		operatorSA = DefaultOperatorServiceAccount
	}
	rules := []any{map[string]any{
		"from": []any{map[string]any{"source": map[string]any{"principals": []any{
			principal(f.Namespace, f.Spec.ServiceAccountName),
			principal(opts.OperatorNamespace, operatorSA),
		}}}},
		"to": []any{map[string]any{"operation": map[string]any{"ports": []any{"8081"}}}},
	}}
	if f.Spec.Mesh.Istio.ApplicationAccess != fleet.ApplicationAccessPolicies {
		rules = append(rules, map[string]any{
			"to": []any{map[string]any{"operation": map[string]any{"ports": []any{"8080"}}}},
		})
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(authorizationPolicyGVK)
	obj.SetName(f.Name + "-mesh")
	obj.SetNamespace(f.Namespace)
	obj.Object["spec"] = map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{FleetLabel: string(f.UID)}},
		"action":   "ALLOW",
		"rules":    rules,
	}
	return obj
}

// reconcileMesh applies the fleet's AuthorizationPolicy before any member
// exists, so no member ever serves its peer port without it. The policy is
// owned by the CelldFleet and garbage-collected after it, which outlives the
// members' SIGTERM handoff.
func (r *Reconciler) reconcileMesh(ctx context.Context, f *fleet.CelldFleet) error {
	if f.Spec.Mesh == nil {
		return nil
	}
	if _, err := r.applyRouting(ctx, f, desiredAuthorizationPolicy(f, r.Options)); err != nil {
		if meta.IsNoMatchError(err) {
			return errors.New("spec.mesh.istio requires Istio's security.istio.io/v1 AuthorizationPolicy API; install Istio 1.22 or later")
		}
		return err
	}
	return nil
}
