package controller

import (
	"context"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testRouting() *fleet.RoutingSpec {
	return &fleet.RoutingSpec{Hostnames: []fleet.RouteHostname{"app.example.com"}, Source: fleet.RoutingSource{Namespace: "edge", PodLabels: map[string]string{"app": "gateway"}}, Ingress: &fleet.IngressRouting{ClassName: "nginx", TLSSecretName: "app-tls", Annotations: map[string]string{"cert-manager.io/cluster-issuer": "issuer"}}}
}
func routeGet(t *testing.T, r *Reconciler, f *fleet.CelldFleet, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	t.Helper()
	obj := routingObject(f, gvk)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func routeCheck(t *testing.T, r *Reconciler, f *fleet.CelldFleet, want string) {
	t.Helper()
	original := r.Client
	r.Client = auditManifestClient(t, original)
	defer func() { r.Client = original }()
	_, reason, msg := r.reconcileRouting(t.Context(), f)
	if reason != want {
		t.Fatalf("want %s, got %s: %s", want, reason, msg)
	}
}
func routingSetup(t *testing.T) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Routing = testRouting()
	r := setup(t, f)
	f = reconcile(t, r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "AwaitingAddress" {
		t.Fatalf("routing condition: %+v", c)
	}
	return r, f
}
func TestRoutingReconcilesPublicIngressAndIsolation(t *testing.T) {
	r, f := routingSetup(t)
	route := routeGet(t, r, f, ingressGVK)
	var ing networkingv1.Ingress
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(route), &ing); err != nil {
		t.Fatal(err)
	}
	if *ing.Spec.IngressClassName != "nginx" || len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "app-tls" || ing.Spec.Rules[0].Host != "app.example.com" || ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number != 8080 || ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != f.Name {
		t.Fatalf("unexpected ingress: %+v", ing.Spec)
	}
	var policy networkingv1.NetworkPolicy
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(route), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].Ports) != 1 || policy.Spec.Ingress[0].Ports[0].Port.IntVal != 8080 || policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "edge" || policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app"] != "gateway" || policy.Spec.PodSelector.MatchLabels[FleetLabel] != string(f.UID) {
		t.Fatalf("unsafe ingress policy: %+v", policy.Spec)
	}
	before := route.GetResourceVersion()
	routeCheck(t, r, f, "AwaitingAddress")
	if after := routeGet(t, r, f, ingressGVK).GetResourceVersion(); before != after {
		t.Fatal("unchanged route was rewritten")
	}
	workloadBefore := workload(f, r.Options)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), workloadBefore); err != nil {
		t.Fatal(err)
	}
	hash := specHash(f)
	annotations := route.GetAnnotations()
	annotations["external.example/note"] = "keep"
	route.SetAnnotations(annotations)
	if err := r.Update(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	f.Spec.Routing.Hostnames = []fleet.RouteHostname{"new.example.com"}
	f.Spec.Routing.Ingress.Annotations = nil
	f.Spec.Routing.Ingress.TLSSecretName = ""
	f.Spec.Routing.Source.Namespace = "new-edge"
	routeCheck(t, r, f, "AwaitingAddress")
	route = routeGet(t, r, f, ingressGVK)
	if route.GetAnnotations()["cert-manager.io/cluster-issuer"] != "" || route.GetAnnotations()["external.example/note"] != "keep" {
		t.Fatal("managed annotation removal clobbered unrelated annotations")
	}
	if _, found, _ := unstructured.NestedSlice(route.Object, "spec", "tls"); found {
		t.Fatal("TLS was not removed")
	}
	if specHash(f) != hash {
		t.Fatal("routing changed storage authority")
	}
	workloadAfter := workload(f, r.Options)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), workloadAfter); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(workloadBefore, workloadAfter) {
		t.Fatal("routing changed the workload")
	}
}

func TestRoutingSwitchAndRemovalWaitForOldRoute(t *testing.T) {
	r, f := routingSetup(t)
	old := routeGet(t, r, f, ingressGVK)
	old.SetFinalizers([]string{"test.example/hold"})
	if err := r.Update(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "public", Namespace: "edge", SectionName: "https"}
	routeCheck(t, r, f, "RemovingRoute")
	obj := routingObject(f, routeGVK)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
		t.Fatalf("replacement created before old route removal: %v", err)
	}
	old = routeGet(t, r, f, ingressGVK)
	old.SetFinalizers(nil)
	if err := r.Update(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	routeCheck(t, r, f, "AwaitingAcceptance")
	route := routeGet(t, r, f, routeGVK)
	refs, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	backend := refs[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	if backend["port"] != int64(8080) || backend["name"] != f.Name {
		t.Fatalf("bad backend: %v", backend)
	}
	f.Spec.Routing = nil
	routeCheck(t, r, f, "RemovingRoute")
	routeCheck(t, r, f, "RemovingRoute")
	routeCheck(t, r, f, "Disabled")
	for _, gvk := range []schema.GroupVersionKind{routeGVK, ingressGVK, routingPolicyGVK} {
		obj := routingObject(f, gvk)
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("resource remains: %s %v", gvk, err)
		}
	}
}

func TestRoutingRejectsForeignOwnershipAndMissingGatewayAPI(t *testing.T) {
	for _, kind := range []schema.GroupVersionKind{ingressGVK, routingPolicyGVK} {
		t.Run(kind.Kind, func(t *testing.T) {
			r, f := routingSetup(t)
			obj := routeGet(t, r, f, kind)
			obj.SetOwnerReferences(nil)
			if err := r.Update(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			routeCheck(t, r, f, "RoutingError")
			f.Spec.Routing = nil
			for range 4 {
				r.reconcileRouting(t.Context(), f)
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatalf("foreign resource removed: %v", err)
			}
		})
	}
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "edge"}
	r := setup(t, f, applicationService(f))
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == routeGVK {
			return &meta.NoKindMatchError{GroupKind: routeGVK.GroupKind(), SearchedVersions: []string{"v1"}}
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	routeCheck(t, r, f, "GatewayAPIUnavailable")
	obj := routingObject(f, routingPolicyGVK)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
		t.Fatal("opened ingress with unavailable routing API")
	}
	f.Spec.Routing = nil
	routeCheck(t, r, f, "Disabled")
}

func TestHTTPRouteReadinessRequiresCurrentMatchingParent(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "edge", Namespace: "network", SectionName: "https"}
	obj := desiredRoute(f)
	obj.SetGeneration(3)
	check := func(want string) {
		t.Helper()
		_, got, _ := routingReadiness(f, obj)
		if got != want {
			t.Fatalf("want %s, got %s", want, got)
		}
	}
	check("AwaitingAcceptance")
	parent := routeParent(f)
	conditions := []any{map[string]any{"type": "Accepted", "status": "True", "observedGeneration": int64(2)}, map[string]any{"type": "ResolvedRefs", "status": "True", "observedGeneration": int64(3)}}
	obj.Object["status"] = map[string]any{"parents": []any{map[string]any{"parentRef": parent, "conditions": conditions}}}
	check("AwaitingAcceptance")
	conditions[0].(map[string]any)["observedGeneration"] = int64(3)
	check("RouteAccepted")
	parent["namespace"] = "wrong"
	check("AwaitingAcceptance")
	parent["namespace"] = "network"
	conditions[1].(map[string]any)["status"] = "False"
	check("RouteRejected")
}

func TestRoutingDeletionCannotRemoveConcurrentReplacement(t *testing.T) {
	r, f := routingSetup(t)
	original := r.Client
	attempted := false
	r.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		options := (&client.DeleteOptions{}).ApplyOptions(opts)
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || *options.Preconditions.UID != obj.GetUID() || *options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
			t.Fatal("routing deletion lacks exact identity/version preconditions")
		}
		attempted = true
		replacement := obj.DeepCopyObject().(client.Object)
		replacement.SetLabels(map[string]string{FleetLabel: "replacement-uid"})
		if err := c.Update(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		return c.Delete(ctx, obj, opts...)
	}})
	f.Spec.Routing = nil
	routeCheck(t, r, f, "RoutingError")
	if !attempted {
		t.Fatal("deletion was not attempted")
	}
	r.Client = original
	route := routeGet(t, r, f, ingressGVK)
	if route.GetLabels()[FleetLabel] != "replacement-uid" {
		t.Fatal("concurrent edit was lost")
	}
	routeGet(t, r, f, routingPolicyGVK)
}

func TestInvalidRoutingDoesNotBlockContraction(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	// Simulate an object admitted by an older schema or bypassing admission.
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Routing = testRouting()
		f.Spec.Routing.Ingress.Annotations = map[string]string{"kubernetes.io/ingress.class": "nginx"}
	})
	f := reconcile(t, x.r, x.f)
	c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady")
	if c == nil || c.Reason != "InvalidConfiguration" {
		t.Fatalf("routing error not reported: %+v", c)
	}
	x.converge()
	if replicas(x.workload()) != 2 {
		t.Fatal("routing prevented contraction")
	}
}

func TestRoutingSwitchPreservesOldRouteOnPreflightFailure(t *testing.T) {
	for _, failure := range []string{"missing API", "foreign route", "foreign policy", "deleting route"} {
		t.Run(failure, func(t *testing.T) {
			r, f := routingSetup(t)
			f.Spec.Routing.Ingress = nil
			f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "public", Namespace: "edge"}
			want := "RoutingError"
			switch failure {
			case "missing API":
				want = "GatewayAPIUnavailable"
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind() == routeGVK {
						return &meta.NoKindMatchError{GroupKind: routeGVK.GroupKind(), SearchedVersions: []string{"v1"}}
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			case "foreign route":
				if err := r.Create(t.Context(), desiredRoute(f)); err != nil {
					t.Fatal(err)
				}
			case "foreign policy":
				obj := routeGet(t, r, f, routingPolicyGVK)
				obj.SetOwnerReferences(nil)
				if err := r.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			case "deleting route":
				obj, err := r.applyRouting(t.Context(), f, desiredRoute(f))
				if err != nil {
					t.Fatal(err)
				}
				obj.SetFinalizers([]string{"test.example/hold"})
				if err := r.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}
			old := routeGet(t, r, f, ingressGVK)
			policy := routeGet(t, r, f, routingPolicyGVK)
			for range 2 {
				routeCheck(t, r, f, want)
			}
			if !equality.Semantic.DeepEqual(old.Object, routeGet(t, r, f, ingressGVK).Object) {
				t.Fatal("old ingress changed despite failed preflight")
			}
			if !equality.Semantic.DeepEqual(policy.Object, routeGet(t, r, f, routingPolicyGVK).Object) {
				t.Fatal("old ingress policy changed despite failed preflight")
			}
		})
	}
}
