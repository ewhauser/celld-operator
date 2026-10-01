package controller

import (
	"context"
	"errors"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Istio sidecar mode (spec.mesh.istio). Under STRICT mTLS every caller of a
// member needs a mesh identity, and once any ALLOW AuthorizationPolicy selects
// the members, Istio denies whatever no ALLOW rule names. The operator
// therefore renders the sidecar, lets it reach istiod, and owns the one policy
// that keeps peer RPC and its own /state reads working: port 8081 accepts only
// the fleet's ServiceAccount and the operator's.

var (
	authorizationPolicyGVK = schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "AuthorizationPolicy"}
	peerAuthenticationGVK  = schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: "PeerAuthentication"}
)

const (
	istioInjectLabel = "sidecar.istio.io/inject"
	// Remember explicit mesh management across removal of spec.mesh. This keeps
	// namespace injection disabled afterward and retains the last control-plane
	// destination until the final sidecar has exited.
	istioHistoryAnnotation = "celld.eric.dev/istio-control-plane"
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
		if f.Annotations[istioHistoryAnnotation] != "" {
			template.Labels[istioInjectLabel] = "false"
		}
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

// rememberMesh records departure information before changing any workload or
// policy. Fleet metadata and the workload have independent resourceVersion CAS.
func (r *Reconciler) rememberMesh(ctx context.Context, f *fleet.CelldFleet) error {
	if f.Spec.Mesh == nil || f.Annotations[istioHistoryAnnotation] == f.Spec.Mesh.Istio.ControlPlaneNamespace {
		return nil
	}
	before := f.DeepCopy()
	if f.Annotations == nil {
		f.Annotations = map[string]string{}
	}
	f.Annotations[istioHistoryAnnotation] = f.Spec.Mesh.Istio.ControlPlaneNamespace
	return r.Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
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

func meshObject(f *fleet.CelldFleet, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(f.Name + "-mesh")
	obj.SetNamespace(f.Namespace)
	return obj
}

// desiredAuthorizationPolicy admits the peer port from the fleet's and the
// operator's identities. While members are mixed, an unmeshed member's
// plaintext peer RPC carries no identity, so the peer rule admits any source;
// the fleet NetworkPolicy still limits 8081 to members and the operator.
func desiredAuthorizationPolicy(f *fleet.CelldFleet, opts Options, mixed bool) *unstructured.Unstructured {
	operatorSA := opts.OperatorServiceAccount
	if operatorSA == "" {
		operatorSA = DefaultOperatorServiceAccount
	}
	peer := map[string]any{"to": []any{map[string]any{"operation": map[string]any{"ports": []any{"8081"}}}}}
	if !mixed {
		peer["from"] = []any{map[string]any{"source": map[string]any{"principals": []any{
			principal(f.Namespace, f.Spec.ServiceAccountName),
			principal(opts.OperatorNamespace, operatorSA),
		}}}}
	}
	rules := []any{peer}
	if f.Spec.Mesh == nil || f.Spec.Mesh.Istio.ApplicationAccess != fleet.ApplicationAccessPolicies {
		rules = append(rules, map[string]any{
			"to": []any{map[string]any{"operation": map[string]any{"ports": []any{"8080"}}}},
		})
	}
	obj := meshObject(f, authorizationPolicyGVK)
	obj.Object["spec"] = map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{FleetLabel: string(f.UID)}},
		"action":   "ALLOW",
		"rules":    rules,
	}
	return obj
}

// desiredPeerAuthentication accepts plaintext on the peer port while members
// are mixed, whatever mTLS mode the mesh or namespace sets, so a meshed member
// still hears from peers that have not rolled yet.
func desiredPeerAuthentication(f *fleet.CelldFleet) *unstructured.Unstructured {
	obj := meshObject(f, peerAuthenticationGVK)
	obj.Object["spec"] = map[string]any{
		"selector":      map[string]any{"matchLabels": map[string]any{FleetLabel: string(f.UID)}},
		"portLevelMtls": map[string]any{"8081": map[string]any{"mode": "PERMISSIVE"}},
	}
	return obj
}

func podTemplateOf(w client.Object) corev1.PodTemplateSpec {
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		return w.Spec.Template
	case *appsv1.Deployment:
		return w.Spec.Template
	}
	return corev1.PodTemplateSpec{}
}

// meshMember reports whether a workload's template puts members in the mesh.
func meshMember(w client.Object) bool {
	return podTemplateOf(w).Labels[istioInjectLabel] == "true"
}

// meshMixed reports whether the members may be a mix of meshed and unmeshed
// Pods: the live template or any live Pod, terminating ones included, differs
// from the desired membership. A fleet without a workload still checks Pods:
// garbage collection can leave a draining proxy after workload deletion.
func (r *Reconciler) meshMixed(ctx context.Context, f *fleet.CelldFleet) (bool, error) {
	want := f.Spec.Mesh != nil
	if !want && f.Annotations[istioHistoryAnnotation] == "" {
		// Preserve the behavior of fleets that never opted in, including those
		// whose administrator independently configured namespace injection.
		return false, nil
	}
	w := emptyObject(workload(f, r.Options))
	err := r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil && w.GetLabels()[FleetLabel] != string(f.UID) {
		// A foreign workload is reported by reconcileWorkload.
		return false, nil
	}
	if err == nil && meshMember(w) != want {
		return true, nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if podHasIstioProxy(&p) != want {
			return true, nil
		}
	}
	return false, nil
}

// Injection labels express intent. The actual proxy may be a regular
// container or a restartable init container (Kubernetes native sidecar).
func podHasIstioProxy(p *corev1.Pod) bool {
	for _, c := range p.Spec.Containers {
		if c.Name == "istio-proxy" {
			return true
		}
	}
	for _, c := range p.Spec.InitContainers {
		if c.Name == "istio-proxy" && c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			return true
		}
	}
	return false
}

// reconcileMesh keeps the peer port working through every membership state.
// A new meshed fleet gets the strict policy before any member exists. Joining
// or leaving the mesh rolls one member at a time like any template change;
// until every member matches, the peer port accepts plaintext from any source
// NetworkPolicy admits, and the strict policy returns once the roll is done.
// The objects are owned by the CelldFleet and garbage-collected after it,
// which outlives the members' SIGTERM handoff.
func (r *Reconciler) reconcileMesh(ctx context.Context, f *fleet.CelldFleet, mixed bool) error {
	if f.Spec.Mesh == nil && !mixed {
		return r.removeMesh(ctx, f)
	}
	if mixed {
		if _, err := r.applyRouting(ctx, f, desiredPeerAuthentication(f)); err != nil {
			return meshAPIError(err)
		}
	}
	if _, err := r.applyRouting(ctx, f, desiredAuthorizationPolicy(f, r.Options, mixed)); err != nil {
		return meshAPIError(err)
	}
	if !mixed {
		if _, err := r.removeOwned(ctx, f, meshObject(f, peerAuthenticationGVK)); err != nil {
			return err
		}
	}
	return nil
}

func meshAPIError(err error) error {
	if meta.IsNoMatchError(err) {
		return errors.New("spec.mesh.istio requires Istio's security.istio.io/v1 AuthorizationPolicy and PeerAuthentication APIs; install Istio 1.22 or later")
	}
	return err
}

// removeMesh deletes the mesh objects of a fleet that has fully left the mesh.
// Clusters without Istio and fleet-namespace Roles from before mesh support
// have none to delete.
func (r *Reconciler) removeMesh(ctx context.Context, f *fleet.CelldFleet) error {
	for _, gvk := range []schema.GroupVersionKind{peerAuthenticationGVK, authorizationPolicyGVK} {
		if _, err := r.removeOwned(ctx, f, meshObject(f, gvk)); err != nil && (!apierrors.IsForbidden(err) || f.Annotations[istioHistoryAnnotation] != "") {
			return err
		}
	}
	return nil
}
