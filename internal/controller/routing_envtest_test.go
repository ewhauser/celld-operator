package controller

import (
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnvtestRoutingAdmissionAndUpdates(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	f := &fleet.CelldFleet{}
	key := client.ObjectKeyFromObject(x.fleet)
	if err := r.Get(t.Context(), key, f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Routing = testRouting()
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatalf("adding routing rejected: %v", err)
	}
	f = reconcile(t, r, f)
	c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady")
	if c == nil || c.Reason != "AwaitingAddress" {
		t.Fatalf("routing failed: %+v", c)
	}
	ingress := &networkingv1.Ingress{}
	routeKey := client.ObjectKey{Namespace: f.Namespace, Name: f.Name + "-routing"}
	if err := r.Get(t.Context(), routeKey, ingress); err != nil {
		t.Fatal(err)
	}
	version := ingress.ResourceVersion
	f = reconcile(t, r, f)
	if err := r.Get(t.Context(), routeKey, ingress); err != nil {
		t.Fatal(err)
	}
	if version != ingress.ResourceVersion {
		t.Fatal("API defaulting caused repeated route writes")
	}
	bad := f.DeepCopy()
	bad.Spec.Routing.Source.PodLabels = nil
	if err := r.Update(t.Context(), bad); !apierrors.IsInvalid(err) {
		t.Fatalf("empty source selector admitted: %v", err)
	}
	bad = f.DeepCopy()
	bad.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "edge"}
	if err := r.Update(t.Context(), bad); !apierrors.IsInvalid(err) {
		t.Fatalf("both modes admitted: %v", err)
	}
	bad = f.DeepCopy()
	bad.Spec.Routing.Hostnames = []fleet.RouteHostname{"app.example.com", "app.example.com"}
	if err := r.Update(t.Context(), bad); !apierrors.IsInvalid(err) {
		t.Fatalf("duplicate hostnames admitted: %v", err)
	}

	for _, annotations := range []map[string]string{
		{"kubernetes.io/ingress.class": "nginx"},
		{"celld.eric.dev/routing-annotation-keys": "[]"},
		{"example.com/oversized": strings.Repeat("a", 4097)},
		{"example.com/oversized": strings.Repeat("é", 2049)},
	} {
		bad := f.DeepCopy()
		bad.Spec.Routing.Ingress.Annotations = annotations
		if err := r.Update(t.Context(), bad); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid annotations admitted: %v", err)
		}
	}
	f.Spec.Routing.Hostnames = []fleet.RouteHostname{"changed.example.com"}
	f.Spec.Routing.Ingress.Annotations = nil
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatalf("routing edit rejected: %v", err)
	}
	f = reconcile(t, r, f)
	if err := r.Get(t.Context(), routeKey, ingress); err != nil {
		t.Fatal(err)
	}
	if ingress.Spec.Rules[0].Host != "changed.example.com" || ingress.Annotations["cert-manager.io/cluster-issuer"] != "" {
		t.Fatal("routing edit was not applied")
	}
	f.Spec.Routing = nil
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatalf("routing removal rejected: %v", err)
	}
	for range 4 {
		f = reconcile(t, r, f)
	}
	if err := r.Get(t.Context(), routeKey, ingress); !apierrors.IsNotFound(err) {
		t.Fatalf("ingress was not removed: %v", err)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "Disabled" {
		t.Fatalf("disabled status: %+v", c)
	}
	// Gateway CRDs are intentionally absent: this is an optional integration and
	// must not prevent the operator from provisioning a fleet or using Ingress.
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "public"}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "GatewayAPIUnavailable" {
		t.Fatalf("missing Gateway API status: %+v", c)
	}
}

// Supply an official standard-channel HTTPRoute CRD to exercise Gateway API
// admission/defaulting without adding optional Gateway APIs to normal envtest.
func TestEnvtestGatewayRouting(t *testing.T) {
	path := os.Getenv("CELLD_GATEWAY_CRD")
	if path == "" {
		t.Skip("set CELLD_GATEWAY_CRD to an official v1 HTTPRoute CRD YAML")
	}
	r, x := envtestSetup(t, "Bucket")
	if _, err := envtest.InstallCRDs(envtestConfig, envtest.CRDInstallOptions{Paths: []string{path}, ErrorIfPathMissing: true}); err != nil {
		t.Fatal(err)
	}
	// Obtain a new RESTMapper after installing the optional API.
	r.Client = envtestClient(t)
	x.provision(t, r)
	f := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "public", Namespace: "edge", SectionName: "https"}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "AwaitingAcceptance" {
		t.Fatalf("HTTPRoute creation: %+v", c)
	}
	obj := routeGet(t, r, f, routeGVK)
	version := obj.GetResourceVersion()
	f = reconcile(t, r, f)
	if obj = routeGet(t, r, f, routeGVK); obj.GetResourceVersion() != version {
		t.Fatal("Gateway API defaulting caused repeated writes")
	}
	conditions := []any{}
	for _, kind := range []string{"Accepted", "ResolvedRefs"} {
		conditions = append(conditions, map[string]any{"type": kind, "status": "True", "reason": kind, "message": "test controller accepted the route", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339), "observedGeneration": obj.GetGeneration()})
	}
	obj.Object["status"] = map[string]any{"parents": []any{map[string]any{"parentRef": routeParent(f), "controllerName": "example.com/test-gateway", "conditions": conditions}}}
	if err := r.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "RouteAccepted" {
		t.Fatalf("acceptance not observed: %+v", c)
	}
	f.Spec.Routing.Hostnames = []fleet.RouteHostname{"changed.example.com"}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "AwaitingAcceptance" {
		t.Fatalf("stale acceptance survived a spec update: %+v", c)
	}
}

func TestEnvtestRoutingSwitchPreservesIngressWithoutGatewayAPI(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	f := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Routing = testRouting()
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	old := routeGet(t, r, f, ingressGVK)
	policy := routeGet(t, r, f, routingPolicyGVK)
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "public"}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f = reconcile(t, r, f)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, "RoutingReady"); c == nil || c.Reason != "GatewayAPIUnavailable" {
		t.Fatalf("missing API not reported: %+v", c)
	}
	if got := routeGet(t, r, f, ingressGVK); got.GetResourceVersion() != old.GetResourceVersion() {
		t.Fatal("old ingress changed")
	}
	if got := routeGet(t, r, f, routingPolicyGVK); got.GetResourceVersion() != policy.GetResourceVersion() {
		t.Fatal("old ingress policy changed")
	}
}
