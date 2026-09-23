package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func previewFixture() *fleet.CelldPreview {
	return &fleet.CelldPreview{
		Name: "pr-42", Namespace: "fleets", UID: "d43be4c2-a643-4fe6-bcad-ad249dc55493", Generation: 1, CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)),
		Spec: fleet.CelldPreviewSpec{Source: "feature/login", TTLSeconds: 3600, FleetRef: fleet.PreviewFleetReference{Name: "shared-previews"}},
	}
}

func previewPoolFixture(p *fleet.CelldPreview) *fleet.CelldFleet {
	parent := fixture(p.Spec.FleetRef.Name, "parent-bucket", "Bucket")
	parent.Namespace = p.Namespace
	parent.UID = "pool-uid"
	parent.Spec.Previews = &fleet.FleetPreviewsSpec{
		RuntimeImage: parent.Spec.RuntimeImage, ServiceAccountName: "preview-runtime",
		Storage: fleet.PreviewStorage{Bucket: "preview-only-bucket", Region: "us-east-1"}, Zone: "us-east-1a",
		Routing: fleet.PreviewRouting{BaseDomain: "previews.example.com", Scheme: "https", Source: fleet.RoutingSource{Namespace: "edge", PodLabels: map[string]string{"app": "gateway"}}, Ingress: &fleet.IngressRouting{ClassName: "nginx", TLSSecretName: "preview-wildcard"}},
	}
	return parent
}

func createEnvtestPool(t *testing.T, c client.Client, p *fleet.CelldPreview) *fleet.CelldFleet {
	t.Helper()
	pool := previewPoolFixture(p)
	pool.UID = ""
	pool.Spec.Previews.Storage.Bucket = p.Namespace + "-previews"
	pool.Spec.Previews.Routing.Scheme = ""
	if err := c.Create(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if pool.Spec.Previews.Routing.Scheme != "https" {
		t.Fatal("pool routing default missing")
	}
	return pool
}

func previewSetup(t *testing.T, p *fleet.CelldPreview) *PreviewReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(envtestScheme(t)).WithStatusSubresource(&fleet.CelldPreview{}, &fleet.CelldFleet{}).WithObjects(p, previewPoolFixture(p)).Build()
	return &PreviewReconciler{Client: c, now: func() time.Time { return p.CreationTimestamp.Add(time.Minute) }}
}

func previewReconcile(t *testing.T, r *PreviewReconciler, p *fleet.CelldPreview) *fleet.CelldPreview {
	t.Helper()
	original := r.Client
	r.Client = auditManifestClient(t, original)
	defer func() { r.Client = original }()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
		t.Fatal(err)
	}
	got := &fleet.CelldPreview{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(p), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPreviewIsolationAndURLs(t *testing.T) {
	p := previewFixture()
	q := p.DeepCopy()
	q.Name = "pr-43"
	q.UID = "d43be4c2-a643-4fe6-bcad-ad249dc55494"
	pool := previewPoolFixture(p)
	for _, gateway := range []bool{false, true} {
		if gateway {
			pool.Spec.Previews.Routing.Ingress = nil
			pool.Spec.Previews.Routing.Gateway = &fleet.GatewayRouting{Name: "edge", Namespace: "edge"}
		}
		a, b := previewFleet(p, pool), previewFleet(q, pool)
		if a.Name == b.Name || a.Spec.Storage.Prefix == b.Spec.Storage.Prefix || a.Spec.Storage.Bucket != b.Spec.Storage.Bucket || a.Spec.Routing.Hostnames[0] == b.Spec.Routing.Hostnames[0] {
			t.Fatal("previews share identity or storage")
		}
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
		route := desiredRoute(a)
		var host, backend string
		if gateway {
			hosts, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
			host = hosts[0]
			rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
			refs := rules[0].(map[string]any)["backendRefs"].([]any)
			backend = refs[0].(map[string]any)["name"].(string)
		} else {
			rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
			rule := rules[0].(map[string]any)
			host = rule["host"].(string)
			paths := rule["http"].(map[string]any)["paths"].([]any)
			backend = paths[0].(map[string]any)["backend"].(map[string]any)["service"].(map[string]any)["name"].(string)
		}
		if host != string(a.Spec.Routing.Hostnames[0]) || backend != a.Name {
			t.Fatalf("route points to wrong preview: %s %s", host, backend)
		}
	}
	r := previewSetup(t, p)
	got := previewReconcile(t, r, p)
	if got.Status.URL != "https://p-d43be4c2a6434fe6bcadad249dc55493.previews.example.com" || got.Status.Phase != "Pending" {
		t.Fatalf("unexpected status: %+v", got.Status)
	}
	child := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: p.Namespace, Name: got.Status.FleetName}, child); err != nil {
		t.Fatal(err)
	}
	if !previewOwned(p, child) || len(child.Finalizers) != 1 || child.Finalizers[0] != Finalizer {
		t.Fatal("missing lifecycle ownership")
	}
	// A second reconcile and status loss must preserve the child and URL.
	got.Status = fleet.CelldPreviewStatus{}
	if err := r.Status().Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, r, p)
	if got.Status.URL == "" {
		t.Fatal("URL not reconstructed")
	}
}

func TestPreviewReadinessAndDrift(t *testing.T) {
	p := previewFixture()
	r := previewSetup(t, p)
	previewReconcile(t, r, p)
	f := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(previewFleet(p, previewPoolFixture(p))), f); err != nil {
		t.Fatal(err)
	}
	f.Generation = 2
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Ready", "RoutingReady"} {
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionTrue, Reason: "Observed", Message: "observed", ObservedGeneration: 1})
	}
	if err := r.Status().Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if got := previewReconcile(t, r, p); got.Status.Phase == "Ready" {
		t.Fatal("stale readiness accepted")
	}
	for i := range f.Status.Conditions {
		f.Status.Conditions[i].ObservedGeneration = 2
	}
	if err := r.Status().Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if got := previewReconcile(t, r, p); got.Status.Phase != "Ready" {
		t.Fatalf("not ready: %+v", got.Status)
	}
	f.Spec.Replicas++
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if got := previewReconcile(t, r, p); got.Status.Conditions[0].Reason != "FleetDrift" {
		t.Fatalf("drift ignored: %+v", got.Status)
	}
}

func TestPreviewExpirationWaitsForFleetFinalizer(t *testing.T) {
	p := previewFixture()
	r := previewSetup(t, p)
	got := previewReconcile(t, r, p)
	r.now = func() time.Time { return p.CreationTimestamp.Add(time.Hour) }
	got = previewReconcile(t, r, got)
	if got.Status.Phase != "Deleting" {
		t.Fatalf("unexpected phase: %s", got.Status.Phase)
	}
	f := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(previewFleet(p, previewPoolFixture(p))), f); err != nil {
		t.Fatal(err)
	}
	if f.DeletionTimestamp.IsZero() || len(f.Finalizers) == 0 {
		t.Fatal("shutdown finalizer bypassed")
	}
	// Simulate the fleet controller completing its existing shutdown protocol.
	f.Finalizers = nil
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, r, p)
	if got.Status.Phase != "Expired" {
		t.Fatal(got.Status.Phase)
	}
	got.Status = fleet.CelldPreviewStatus{}
	if err := r.Status().Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if got = previewReconcile(t, r, p); got.Status.Phase != "Expired" {
		t.Fatal("expired preview resurrected")
	}
	if err := r.Delete(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(p), &fleet.CelldPreview{}); !apierrors.IsNotFound(err) {
		t.Fatalf("preview remains: %v", err)
	}
}

func TestPreviewRejectsForeignFleetAndMissingChild(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		p := previewFixture()
		r := previewSetup(t, p)
		f := previewFleet(p, previewPoolFixture(p))
		if foreign {
			f.OwnerReferences[0].UID = types.UID("foreign")
			if err := r.Create(t.Context(), f); err != nil {
				t.Fatal(err)
			}
		} else {
			p.Annotations = map[string]string{previewCreated: "true"}
			if err := r.Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		}
		got := previewReconcile(t, r, p)
		if got.Status.Phase != "Blocked" {
			t.Fatalf("unsafe provisioning: %+v", got.Status)
		}
	}
}

func TestEnvtestPreviewAdmissionAndReconcile(t *testing.T) {
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "preview-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	p := previewFixture()
	p.Namespace = ns.Name
	p.UID = ""
	p.ResourceVersion = ""
	p.CreationTimestamp = metav1.Time{}
	p.Spec.TTLSeconds = 0
	pool := createEnvtestPool(t, c, p)
	if err := c.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.TTLSeconds != 86400 {
		t.Fatal("defaults missing")
	}
	r := &PreviewReconciler{Client: c}
	got := previewReconcile(t, r, p)
	if got.Status.URL == "" || got.Status.Phase != "Pending" {
		t.Fatalf("unexpected status: %+v", got.Status)
	}
	for _, mutate := range []func(*fleet.CelldPreview){
		func(p *fleet.CelldPreview) { p.Spec.TTLSeconds++ },
		func(p *fleet.CelldPreview) { p.Spec.FleetRef.Name = "another-pool" },
	} {
		changed := got.DeepCopy()
		mutate(changed)
		if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("immutable change accepted: %v", err)
		}
	}
	changedPool := pool.DeepCopy()
	changedPool.Spec.Previews.Routing.BaseDomain = "changed.example.com"
	if err := c.Update(t.Context(), changedPool); !apierrors.IsInvalid(err) {
		t.Fatalf("pool configuration mutated: %v", err)
	}
	got.Spec.Revision = "new-commit"
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, r, p)
	if got.Status.Phase == "Blocked" {
		t.Fatalf("defaulted child drifts: %+v", got.Status)
	}
}

func TestPreviewCreationRejectionAndAmbiguousFailure(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		p := previewFixture()
		r := previewSetup(t, p)
		base := r.Client
		r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*fleet.CelldFleet); !ok {
				return c.Create(ctx, obj, opts...)
			}
			if rejected {
				return apierrors.NewForbidden(schema.GroupResource{Group: fleet.GroupVersion.Group, Resource: "celldfleets"}, "child", errors.New("namespace role missing"))
			}
			return errors.New("unknown request outcome")
		}})
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		if rejected && err != nil || !rejected && err == nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.Client = base
		got := previewReconcile(t, r, p)
		if rejected && got.Status.Phase != "Pending" || !rejected && got.Status.Conditions[0].Reason != "FleetMissing" {
			t.Fatalf("unsafe retry: %+v", got.Status)
		}
	}
}

func TestPreviewExplicitDeletionAndDeletePreconditions(t *testing.T) {
	p := previewFixture()
	r := previewSetup(t, p)
	got := previewReconcile(t, r, p)
	if err := r.Delete(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	checked := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		o := (&client.DeleteOptions{}).ApplyOptions(opts)
		if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != obj.GetUID() || o.Preconditions.ResourceVersion == nil || *o.Preconditions.ResourceVersion != obj.GetResourceVersion() {
			t.Fatal("delete lacks identity preconditions")
		}
		checked = true
		return c.Delete(ctx, obj, opts...)
	}})
	got = previewReconcile(t, r, p)
	if !checked || got.Status.Phase != "Deleting" || len(got.Finalizers) == 0 {
		t.Fatalf("deletion did not wait for child: %+v", got)
	}
}

func TestEnvtestPreviewRoutesAndRejectedConfigurations(t *testing.T) {
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "preview-routes-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	newPreview := func(name string) *fleet.CelldPreview {
		p := previewFixture()
		p.Name = name
		p.Namespace = ns.Name
		p.UID = ""
		p.ResourceVersion = ""
		p.CreationTimestamp = metav1.Time{}
		return p
	}
	createEnvtestPool(t, c, newPreview("one"))
	for i, mutate := range []func(*fleet.CelldPreview){
		func(p *fleet.CelldPreview) { p.Spec.FleetRef.Name = "Bad_Name" },
		func(p *fleet.CelldPreview) { p.Spec.TTLSeconds = 1 },
	} {
		p := newPreview("invalid")
		mutate(p)
		if err := c.Create(t.Context(), p); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid case %d admitted: %v", i, err)
		}
	}
	for i, mutate := range []func(*fleet.CelldFleet){
		func(p *fleet.CelldFleet) { p.Spec.Previews.Routing.BaseDomain = "*.example.com" },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Routing.Ingress = nil },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Routing.Gateway = &fleet.GatewayRouting{Name: "edge"} },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Routing.Ingress.TLSSecretName = "" },
	} {
		p := previewPoolFixture(newPreview("unused"))
		p.Name = "invalid"
		p.UID = ""
		mutate(p)
		if err := c.Create(t.Context(), p); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid pool case %d admitted: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for _, name := range []string{"one", "two"} {
		p := newPreview(name)
		if err := c.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		pr := &PreviewReconciler{Client: c}
		got := previewReconcile(t, pr, p)
		f := &fleet.CelldFleet{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: got.Status.FleetName}, f); err != nil {
			t.Fatal(err)
		}
		// Exercise the production fleet routing reconciler against real Services,
		// NetworkPolicies and Ingress API admission, without a kubelet/data plane.
		fr := &Reconciler{Client: c, NetworkPolicyEnforced: true}
		for _, obj := range prerequisites(f, Options{}) {
			if _, ok := obj.(*corev1.Service); ok {
				if err := c.Create(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := fr.reportRouting(t.Context(), client.ObjectKeyFromObject(f)); err != nil {
			t.Fatal(err)
		}
		route := &networkingv1.Ingress{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns.Name, Name: f.Name + "-routing"}, route); err != nil {
			t.Fatal(err)
		}
		host := route.Spec.Rules[0].Host
		if seen[host] || got.Status.URL != "https://"+host || route.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != f.Name {
			t.Fatalf("wrong or shared URL: %+v", route.Spec)
		}
		seen[host] = true
		// Expiry requests child deletion; the fleet controller removes the route
		// while retaining authority for safe runtime shutdown.
		pr.now = func() time.Time { return p.CreationTimestamp.Add(2 * time.Hour) }
		previewReconcile(t, pr, p)
		for range 3 {
			if err := fr.reportRouting(t.Context(), client.ObjectKeyFromObject(f)); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(route), route); !apierrors.IsNotFound(err) {
			t.Fatalf("expired route remains: %v", err)
		}
	}
}

func TestPreviewRequiresEnabledSeparateParentStorage(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared-parent-bucket", true: "disabled"}[disabled], func(t *testing.T) {
			p := previewFixture()
			r := previewSetup(t, p)
			parent := &fleet.CelldFleet{}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.FleetRef.Name}, parent); err != nil {
				t.Fatal(err)
			}
			if disabled {
				parent.Spec.Previews = nil
			} else {
				parent.Spec.Previews.Storage.Bucket = parent.Spec.Storage.Bucket
			}
			if err := r.Update(t.Context(), parent); err != nil {
				t.Fatal(err)
			}
			got := previewReconcile(t, r, p)
			if got.Status.Phase != "Blocked" {
				t.Fatal("invalid parent admitted preview")
			}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: p.Namespace, Name: previewName(p)}, &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
				t.Fatal("child created")
			}
		})
	}
}
