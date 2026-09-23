package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	routeGVK         = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	ingressGVK       = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}
	routingPolicyGVK = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
)

const routingAnnotations = "celld.eric.dev/routing-annotation-keys"

func routingObject(f *fleet.CelldFleet, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(f.Name + "-routing")
	obj.SetNamespace(f.Namespace)
	return obj
}

func routingOwned(f *fleet.CelldFleet, obj client.Object) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.UID == f.UID && owner.Name == f.Name && owner.Kind == "CelldFleet" && owner.APIVersion == fleet.GroupVersion.String() && obj.GetLabels()[FleetLabel] == string(f.UID)
}

func routeParent(f *fleet.CelldFleet) map[string]any {
	g := f.Spec.Routing.Gateway
	namespace := g.Namespace
	if namespace == "" {
		namespace = f.Namespace
	}
	parent := map[string]any{"group": routeGVK.Group, "kind": "Gateway", "name": g.Name, "namespace": namespace}
	if g.SectionName != "" {
		parent["sectionName"] = g.SectionName
	}
	return parent
}

func desiredRoute(f *fleet.CelldFleet) *unstructured.Unstructured {
	cfg := f.Spec.Routing
	hosts := make([]any, len(cfg.Hostnames))
	for n, h := range cfg.Hostnames {
		hosts[n] = string(h)
	}
	if cfg.Gateway != nil {
		obj := routingObject(f, routeGVK)
		obj.Object["spec"] = map[string]any{
			"parentRefs": []any{routeParent(f)}, "hostnames": hosts,
			"rules": []any{map[string]any{
				"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
				"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": f.Name, "port": int64(8080), "weight": int64(1)}},
			}},
		}
		return obj
	}
	obj := routingObject(f, ingressGVK)
	obj.SetAnnotations(maps.Clone(cfg.Ingress.Annotations))
	rules := make([]any, len(hosts))
	for n, h := range hosts {
		rules[n] = map[string]any{"host": h, "http": map[string]any{"paths": []any{map[string]any{
			"path": "/", "pathType": "Prefix", "backend": map[string]any{"service": map[string]any{"name": f.Name, "port": map[string]any{"number": int64(8080)}}},
		}}}}
	}
	spec := map[string]any{"ingressClassName": cfg.Ingress.ClassName, "rules": rules}
	if cfg.Ingress.TLSSecretName != "" {
		spec["tls"] = []any{map[string]any{"hosts": hosts, "secretName": cfg.Ingress.TLSSecretName}}
	}
	obj.Object["spec"] = spec
	return obj
}

func desiredRoutingPolicy(f *fleet.CelldFleet) *unstructured.Unstructured {
	cfg := f.Spec.Routing.Source
	podLabels := map[string]any{}
	for k, v := range cfg.PodLabels {
		podLabels[k] = v
	}
	obj := routingObject(f, routingPolicyGVK)
	obj.Object["spec"] = map[string]any{
		"podSelector": map[string]any{"matchLabels": map[string]any{FleetLabel: string(f.UID)}},
		"policyTypes": []any{"Ingress"},
		"ingress": []any{map[string]any{
			"from": []any{map[string]any{
				"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": cfg.Namespace}},
				"podSelector":       map[string]any{"matchLabels": podLabels},
			}},
			"ports": []any{map[string]any{"protocol": "TCP", "port": int64(8080)}},
		}},
	}
	return obj
}

// Routing has separate ownership from workloads and storage. It may be updated
// or garbage-collected without granting any runtime lifecycle authority.
func (r *Reconciler) applyRouting(ctx context.Context, f *fleet.CelldFleet, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired.SetLabels(labels(f))
	owner := metav1.NewControllerRef(f, fleet.GroupVersion.WithKind("CelldFleet"))
	owner.BlockOwnerDeletion = new(false) // Routing never holds the fleet lifecycle finalizer.
	desired.SetOwnerReferences([]metav1.OwnerReference{*owner})
	annotations := desired.GetAnnotations()
	keys := slices.Sorted(maps.Keys(annotations))
	encoded, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[routingAnnotations] = string(encoded)
	desired.SetAnnotations(annotations)
	actual := routingObject(f, desired.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(actual), actual); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		return desired, r.Create(ctx, desired)
	}
	if !routingOwned(f, actual) {
		return nil, fmt.Errorf("%s %s is not owned by this fleet UID", actual.GetKind(), actual.GetName())
	}
	if !actual.GetDeletionTimestamp().IsZero() {
		return nil, fmt.Errorf("%s %s is still deleting", actual.GetKind(), actual.GetName())
	}
	before := actual.DeepCopy()
	var oldKeys []string
	merged := actual.GetAnnotations()
	if merged == nil {
		merged = map[string]string{}
	}
	if stored := merged[routingAnnotations]; stored != "" {
		if err := json.Unmarshal([]byte(stored), &oldKeys); err != nil {
			return nil, fmt.Errorf("invalid managed routing annotation inventory")
		}
	}
	for _, k := range oldKeys {
		delete(merged, k)
	}
	maps.Copy(merged, annotations)
	actual.SetAnnotations(merged)
	actual.Object["spec"] = desired.Object["spec"]
	if equality.Semantic.DeepEqual(before.Object, actual.Object) {
		return actual, nil
	}
	// ResourceVersion fences concurrent edits, including an ownership change.
	return actual, r.Update(ctx, actual)
}

// Return true only after absence is observed. Finalizers must finish before
// replacing a route kind or closing its supporting NetworkPolicy.
func (r *Reconciler) removeRouting(ctx context.Context, f *fleet.CelldFleet, gvk schema.GroupVersionKind) (bool, error) {
	obj := routingObject(f, gvk)
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, err
	}
	if !routingOwned(f, obj) {
		return true, nil
	}
	if obj.GetDeletionTimestamp().IsZero() {
		uid, rv := obj.GetUID(), obj.GetResourceVersion()
		if err := r.Delete(ctx, obj, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

func (r *Reconciler) reconcileRouting(ctx context.Context, f *fleet.CelldFleet) (metav1.ConditionStatus, string, string) {
	if f.DeletionTimestamp.IsZero() {
		if err := f.Validate(); err != nil {
			return metav1.ConditionFalse, "InvalidConfiguration", err.Error()
		}
	}
	enabled := f.Spec.Routing != nil && f.DeletionTimestamp.IsZero()
	keep := schema.GroupVersionKind{}
	if enabled {
		keep = ingressGVK
		if f.Spec.Routing.Gateway != nil {
			keep = routeGVK
		}

		// Check the replacement before touching the currently serving route or its
		// policy. A missing API or ownership conflict must not turn an edit into an outage.
		if !r.NetworkPolicyEnforced {
			return metav1.ConditionFalse, "IsolationUnverified", "Verify NetworkPolicy enforcement before enabling routing"
		}
		svc := &corev1.Service{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), svc); err != nil {
			return routingFailure(err)
		}
		if !matches(applicationService(f), svc) {
			return metav1.ConditionFalse, "ServiceConflict", "Application Service does not match the fleet"
		}
		for _, gvk := range []schema.GroupVersionKind{keep, routingPolicyGVK} {
			probe := routingObject(f, gvk)
			err := r.Get(ctx, client.ObjectKeyFromObject(probe), probe)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return routingFailure(err)
			}
			if !routingOwned(f, probe) {
				return metav1.ConditionFalse, "RoutingError", gvk.Kind + " name is already owned by another resource"
			}
			if !probe.GetDeletionTimestamp().IsZero() {
				return metav1.ConditionFalse, "RoutingError", "Waiting for selected " + gvk.Kind + " deletion"
			}
		}
	}
	// Clear the previous kind before creating the new route. Unknown APIs are
	// harmless when unused; selected Gateway APIs must exist before permitting traffic.
	for _, gvk := range []schema.GroupVersionKind{ingressGVK, routeGVK} {
		if gvk == keep {
			continue
		}
		absent, err := r.removeRouting(ctx, f, gvk)
		if err != nil {
			return routingFailure(err)
		}
		if !absent {
			return metav1.ConditionFalse, "RemovingRoute", "Waiting for the previous route to disappear"
		}
	}
	if !enabled {
		absent, err := r.removeRouting(ctx, f, routingPolicyGVK)
		if err != nil {
			return routingFailure(err)
		}
		if !absent {
			return metav1.ConditionFalse, "RemovingRoute", "Waiting for routing NetworkPolicy deletion"
		}
		return metav1.ConditionTrue, "Disabled", "Optional routing is disabled"
	}
	if _, err := r.applyRouting(ctx, f, desiredRoutingPolicy(f)); err != nil {
		return routingFailure(err)
	}
	route, err := r.applyRouting(ctx, f, desiredRoute(f))
	if err != nil {
		return routingFailure(err)
	}
	return routingReadiness(f, route)
}

func routingFailure(err error) (metav1.ConditionStatus, string, string) {
	if meta.IsNoMatchError(err) {
		return metav1.ConditionFalse, "GatewayAPIUnavailable", "Install Gateway API v1 HTTPRoute CRDs and a Gateway controller"
	}
	return metav1.ConditionFalse, "RoutingError", err.Error()
}

func routingReadiness(f *fleet.CelldFleet, route *unstructured.Unstructured) (metav1.ConditionStatus, string, string) {
	if route.GroupVersionKind() == ingressGVK {
		addresses, _, _ := unstructured.NestedSlice(route.Object, "status", "loadBalancer", "ingress")
		for _, item := range addresses {
			address, _ := item.(map[string]any)
			ip, _, _ := unstructured.NestedString(address, "ip")
			host, _, _ := unstructured.NestedString(address, "hostname")
			if ip != "" || host != "" {
				return metav1.ConditionTrue, "AddressAssigned", "Ingress controller reports an address; verify DNS, TLS and application connectivity"
			}
		}
		return metav1.ConditionUnknown, "AwaitingAddress", "Ingress is configured; its controller has not reported an address"
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	wanted := routeParent(f)
	for _, item := range parents {
		parent, _ := item.(map[string]any)
		ref, _, _ := unstructured.NestedMap(parent, "parentRef")
		if ref == nil {
			continue
		}
		if _, ok := ref["namespace"]; !ok {
			ref["namespace"] = f.Namespace
		}
		if _, ok := ref["group"]; !ok {
			ref["group"] = routeGVK.Group
		}
		if _, ok := ref["kind"]; !ok {
			ref["kind"] = "Gateway"
		}
		if !equality.Semantic.DeepEqual(wanted, ref) {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(parent, "conditions")
		accepted, resolved := false, false
		for _, item := range conditions {
			c, _ := item.(map[string]any)
			generation, _, _ := unstructured.NestedInt64(c, "observedGeneration")
			if generation != route.GetGeneration() {
				continue
			}
			kind, _, _ := unstructured.NestedString(c, "type")
			status, _, _ := unstructured.NestedString(c, "status")
			if kind != "Accepted" && kind != "ResolvedRefs" {
				continue
			}
			if status == "False" {
				return metav1.ConditionFalse, "RouteRejected", "Gateway has rejected the route or its references; inspect HTTPRoute status"
			}
			if kind == "Accepted" {
				accepted = status == "True"
			}
			if kind == "ResolvedRefs" {
				resolved = status == "True"
			}
		}
		if accepted && resolved {
			return metav1.ConditionTrue, "RouteAccepted", "Gateway accepted the current HTTPRoute and resolved its references; verify listener, DNS and TLS"
		}
	}
	return metav1.ConditionUnknown, "AwaitingAcceptance", "Waiting for current-generation Gateway acceptance and resolved references"
}

func (r *Reconciler) reportRouting(ctx context.Context, key client.ObjectKey) error {
	f := &fleet.CelldFleet{}
	if err := r.Get(ctx, key, f); err != nil {
		return client.IgnoreNotFound(err)
	}
	f.Default()
	before := f.DeepCopy()
	status, reason, message := r.reconcileRouting(ctx, f)
	if reason == "Disabled" && meta.FindStatusCondition(f.Status.Conditions, "RoutingReady") == nil {
		return nil
	}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: "RoutingReady", Status: status, Reason: reason, Message: message, ObservedGeneration: f.Generation})
	if equality.Semantic.DeepEqual(before.Status, f.Status) {
		return nil
	}
	return r.Status().Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
