package controller

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPreviewPoolAtomicBucketReservation(t *testing.T) {
	// Two controllers racing for the root must never both acquire it, including
	// the dedicated-fleet path which has no awareness of preview pools.
	for _, dedicated := range []bool{false, true} {
		p := previewPoolFixture(previewFixture())
		q := p.DeepCopy()
		q.Name = "other"
		q.UID = "other-uid"
		c := fake.NewClientBuilder().WithScheme(envtestScheme(t)).Build()
		outcomes := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Go(func() { outcomes <- reservePreviewPool(t.Context(), c, p) })
		wg.Go(func() {
			if dedicated {
				f := fixture("dedicated", p.Spec.Previews.Storage.Bucket, "Bucket")
				outcomes <- c.Create(t.Context(), &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)})
			} else {
				outcomes <- reservePreviewPool(t.Context(), c, q)
			}
		})
		wg.Wait()
		close(outcomes)
		winners := 0
		for err := range outcomes {
			if err == nil {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("expected exactly one root owner, got %d", winners)
		}
	}
}

func TestPreviewPoolScopeIdentity(t *testing.T) {
	p := previewPoolFixture(previewFixture())
	c := fake.NewClientBuilder().WithScheme(envtestScheme(t)).Build()
	if err := reservePreviewPool(t.Context(), c, p); err != nil {
		t.Fatal(err)
	}
	if err := reservePreviewPool(t.Context(), c, p); err != nil {
		t.Fatal(err)
	}
	f := previewFleet(previewFixture(), p)
	if err := verifySharedStorage(t.Context(), c, f); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fleet.CelldFleet){
		func(f *fleet.CelldFleet) { f.Spec.Storage.PreviewFleetRef.UID = "recreated" },
		func(f *fleet.CelldFleet) { f.Namespace = "another" },
		func(f *fleet.CelldFleet) { f.Spec.Storage.PreviewFleetRef.Name = "other" },
		func(f *fleet.CelldFleet) {
			f.Spec.Storage.Endpoint = &fleet.ObjectStoreEndpoint{URL: "http://other:9000"}
		},
	} {
		changed := f.DeepCopy()
		mutate(changed)
		if verifySharedStorage(t.Context(), c, changed) == nil {
			t.Fatal("changed storage authority accepted")
		}
	}
	q := p.DeepCopy()
	q.UID = "recreated"
	if reservePreviewPool(t.Context(), c, q) == nil {
		t.Fatal("pool identity reused")
	}
	q = p.DeepCopy()
	q.Spec.Previews.Zone = "us-east-1b"
	if reservePreviewPool(t.Context(), c, q) == nil {
		t.Fatal("pool configuration drift accepted")
	}
	res := &fleet.CelldStorageReservation{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: bucketReservationName(p.Spec.Previews.Storage.Bucket)}, res); err != nil {
		t.Fatal(err)
	}
	res.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
	if err := c.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	if verifySharedStorage(t.Context(), c, f) == nil {
		t.Fatal("garbage-collectable root accepted")
	}
	if err := c.Delete(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	if verifySharedStorage(t.Context(), c, f) == nil {
		t.Fatal("missing root accepted")
	}
}

func TestPreviewSharedStorageReconciliation(t *testing.T) {
	pool := previewPoolFixture(previewFixture())
	pool.Spec.Previews.ServiceAccountName = "runtime"
	a := previewFleet(previewFixture(), pool)
	a.UID = "fleet-a"
	b := a.DeepCopy()
	b.Name = "preview-b"
	b.UID = "fleet-b"
	b.Spec.Storage.Prefix = "preview-b"
	r := setup(t, a, b)
	if err := reservePreviewPool(t.Context(), r.Client, pool); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fleet.CelldFleet{a, b} {
		reconcile(t, r, f)
		res := &fleet.CelldStorageReservation{}
		if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
			t.Fatal(err)
		}
		if res.Spec.Prefix != f.Spec.Storage.Prefix || res.Spec.FleetUID != string(f.UID) {
			t.Fatal("incorrect prefix owner")
		}
	}
	collision := a.DeepCopy()
	collision.Name = "collision"
	collision.UID = "collision"
	collision.ResourceVersion = ""
	if err := r.Create(t.Context(), collision); err != nil {
		t.Fatal(err)
	}
	got := reconcile(t, r, collision)
	if meta.FindStatusCondition(got.Status.Conditions, "Ready").Reason != "StorageScopeConflict" {
		t.Fatalf("prefix reused: %+v", got.Status)
	}
	dedicated := fixture("dedicated", pool.Spec.Previews.Storage.Bucket, "Bucket")
	if err := r.Create(t.Context(), dedicated); err != nil {
		t.Fatal(err)
	}
	got = reconcile(t, r, dedicated)
	if meta.FindStatusCondition(got.Status.Conditions, "Ready").Reason != "StorageScopeConflict" {
		t.Fatalf("pool bucket claimed: %+v", got.Status)
	}
}

func TestPreviewPoolDeletionDoesNotObstructExpiry(t *testing.T) {
	p := previewFixture()
	r := previewSetup(t, p)
	got := previewReconcile(t, r, p)
	originalURL := got.Status.URL
	pool := previewPoolFixture(p)
	if err := r.Delete(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, r, p)
	if got.Status.Conditions[0].Reason != "PoolMissing" || got.Status.URL != originalURL {
		t.Fatal("pool loss not reported with stable URL")
	}
	pool.UID = "replacement"
	pool.ResourceVersion = ""
	if err := r.Create(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, r, p)
	if got.Status.Conditions[0].Reason != "PoolIdentityChanged" {
		t.Fatal("new pool adopted old preview")
	}
	if err := r.Delete(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return p.CreationTimestamp.Add(2 * time.Hour) }
	got = previewReconcile(t, r, p)
	if got.Status.Phase != "Deleting" || got.Status.URL != originalURL {
		t.Fatal("pool loss prevented expiry")
	}
}

func TestPreviewPoolRendersSmallRuntimeAndSharedStore(t *testing.T) {
	p := previewPoolFixture(previewFixture())
	p.Spec.Previews.Storage.Endpoint = &fleet.ObjectStoreEndpoint{URL: "http://preview-store.fleets.svc:9000", CredentialsSecretName: "preview-store", Egress: fleet.CollectorEgress{Namespace: "fleets", PodLabels: map[string]string{"app": "preview-store"}}}
	f := previewFleet(previewFixture(), p)
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := podTemplate(f, Options{}).Spec
	runtime := pod.Containers[0]
	for key, want := range map[string]string{"CELLD_BUCKET": f.Spec.Storage.URL(), "S3_ENDPOINT": p.Spec.Previews.Storage.Endpoint.URL, "AWS_ALLOW_HTTP": "true", "CELLD_MAX_RESIDENT_CELLS": "8", "CELLD_LOCAL_CACHE_MAX_BYTES": "134217728"} {
		if got, _ := envValue(runtime.Env, key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	for name, key := range map[string]string{"AWS_ACCESS_KEY_ID": "accessKeyId", "AWS_SECRET_ACCESS_KEY": "secretAccessKey"} {
		found := false
		for _, e := range runtime.Env {
			if e.Name == name {
				found = true
				if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != "preview-store" || e.ValueFrom.SecretKeyRef.Key != key {
					t.Fatal("credential not secret-backed")
				}
			}
		}
		if !found {
			t.Fatal("credential reference missing")
		}
	}
	for name, want := range map[corev1.ResourceName]string{corev1.ResourceCPU: "25m", corev1.ResourceMemory: "64Mi", corev1.ResourceEphemeralStorage: "64Mi"} {
		if value := runtime.Resources.Requests[name]; value.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("unexpected %s request", name)
		}
	}
	if runtime.Resources.Limits.Memory().Cmp(resource.MustParse("256Mi")) != 0 || runtime.Resources.Limits.StorageEphemeral().Cmp(resource.MustParse("512Mi")) != 0 || pod.Volumes[0].EmptyDir.Medium != "" || pod.Volumes[0].EmptyDir.SizeLimit.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Fatal("preview storage/memory limits wrong")
	}
	found := false
	for _, o := range prerequisites(f, Options{}) {
		if pdb, ok := o.(*policyv1.PodDisruptionBudget); ok && pdb.Spec.MaxUnavailable.IntValue() != 1 {
			t.Fatal("preview budget must allow one voluntary disruption at a time")
		}
		if policy, ok := o.(*networkingv1.NetworkPolicy); ok {
			rule := policy.Spec.Egress[len(policy.Spec.Egress)-1]
			if rule.Ports[0].Port.IntValue() != 9000 || rule.To[0].PodSelector.MatchLabels["app"] != "preview-store" || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "fleets" {
				t.Fatal("store egress missing or broad")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("policy missing")
	}
	p.Spec.Previews.Scratch = &fleet.ScratchSpec{Request: "16Mi", Limit: "64Mi"}
	p.Spec.Previews.Storage.Endpoint.URL = "https://preview-store.fleets.svc:9000"
	pod = podTemplate(previewFleet(previewFixture(), p), Options{}).Spec
	if got, _ := envValue(pod.Containers[0].Env, "CELLD_LOCAL_CACHE_MAX_BYTES"); got != "16777216" {
		t.Fatal("cache exceeds custom scratch budget")
	}
	if _, ok := envValue(pod.Containers[0].Env, "AWS_ALLOW_HTTP"); ok {
		t.Fatal("HTTPS enables plaintext")
	}
}

func TestEnvtestPreviewStorageAdmission(t *testing.T) {
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "preview-storage-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	p := previewFixture()
	p.Namespace = ns.Name
	pool := previewPoolFixture(p)
	pool.UID = ""
	pool.Spec.Previews.Storage.Endpoint = &fleet.ObjectStoreEndpoint{URL: "http://store:9000", CredentialsSecretName: "store", Egress: fleet.CollectorEgress{PodLabels: map[string]string{"app": "store"}}}
	pool.Spec.Previews.Scratch = &fleet.ScratchSpec{Request: "64Mi", Limit: "512Mi"}
	for _, mutate := range []func(*fleet.CelldFleet){
		func(p *fleet.CelldFleet) { p.Spec.Previews.Storage.Bucket = p.Spec.Storage.Bucket },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Scratch.Request = "0" },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Scratch.Limit = "32Mi" },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Zone = "us-west-2a" },
		func(p *fleet.CelldFleet) { p.Spec.Previews.Storage.Endpoint.URL = "http://store/other" },
	} {
		invalid := pool.DeepCopy()
		mutate(invalid)
		if err := c.Create(t.Context(), invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid pool admitted: %v", err)
		}
	}
	if err := c.Create(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := reservePreviewPool(t.Context(), c, pool); err != nil {
		t.Fatal(err)
	}
	f := previewFleet(p, pool)
	f.OwnerReferences = nil
	if err := c.Create(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := verifySharedStorage(t.Context(), c, f); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fleet.CelldFleet){
		func(f *fleet.CelldFleet) { f.Spec.Storage.Prefix = "other" },
		func(f *fleet.CelldFleet) { f.Spec.Storage.PreviewFleetRef.UID = "other" },
		func(f *fleet.CelldFleet) { f.Spec.Storage.Endpoint.URL = "http://other:9000" },
		func(f *fleet.CelldFleet) { f.Spec.Storage.Scratch.Limit = "1Gi" },
	} {
		changed := f.DeepCopy()
		mutate(changed)
		if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("storage identity mutated: %v", err)
		}
	}
	for _, mutate := range []func(*fleet.CelldFleet){
		func(f *fleet.CelldFleet) { f.Spec.Storage.Prefix = "parent/child" },
		func(f *fleet.CelldFleet) { f.Spec.Storage.PreviewFleetRef = nil },
		func(f *fleet.CelldFleet) { f.Spec.Storage.Prefix = "" },
	} {
		changed := f.DeepCopy()
		changed.Name = "invalid"
		changed.ResourceVersion = ""
		changed.UID = ""
		mutate(changed)
		if err := c.Create(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid scope admitted: %v", err)
		}
	}
}

func TestEnvtestPreviewSampleManifests(t *testing.T) {
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "preview-samples-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preview-store.yaml", "fleet-previews.yaml", "preview.yaml"} {
		f, err := os.Open(filepath.Join("..", "..", "config", "samples", name))
		if err != nil {
			t.Fatal(err)
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for {
			obj := &unstructured.Unstructured{}
			err := decoder.Decode(obj)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = f.Close()
				t.Fatal(err)
			}
			obj.SetNamespace(ns.Name)
			if obj.GetKind() == "CelldFleet" {
				if err := unstructured.SetNestedField(obj.Object, ns.Name, "spec", "storage", "bucket"); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Create(t.Context(), obj); err != nil {
				_ = f.Close()
				t.Fatalf("%s: %v", name, err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	p := &fleet.CelldPreview{Name: "pr-42", Namespace: ns.Name}
	got := previewReconcile(t, &PreviewReconciler{Client: c}, p)
	if got.Status.Phase != "Pending" || got.Status.StorageURL == "" || got.Status.URL == "" {
		t.Fatalf("sample failed to provision: %+v", got.Status)
	}
}

func TestEnablePreviewsPreservesParentRuntime(t *testing.T) {
	parent := fixture("parent", "parent-runtime", "Bucket")
	before := parent.DeepCopy()
	reservation := &fleet.CelldStorageReservation{Spec: fleetReservationSpec(parent)}
	parent.Spec.Previews = previewPoolFixture(previewFixture()).Spec.Previews.DeepCopy()
	if specHash(parent) != specHash(before) {
		t.Fatal("enabling previews invalidated parent reservation")
	}
	if !(&Reconciler{}).reservationMatches(t.Context(), parent, &loadedState{res: reservation}, fleetReservationSpec(parent)) {
		t.Fatal("parent storage authority changed")
	}
	a, b := podTemplate(before, Options{}), podTemplate(parent, Options{})
	if !equality.Semantic.DeepEqual(a, b) {
		t.Fatal("enabling previews changed parent runtime")
	}
}

func TestEnvtestEnablePreviewsOnExistingFleet(t *testing.T) {
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "enable-previews-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	parent := fixture("parent", ns.Name, "Bucket")
	parent.Namespace = ns.Name
	parent.UID = ""
	if err := c.Create(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	hash := specHash(parent)
	parent.Spec.Previews = previewPoolFixture(previewFixture()).Spec.Previews.DeepCopy()
	if err := c.Update(t.Context(), parent); err != nil {
		t.Fatalf("cannot enable previews: %v", err)
	}
	if specHash(parent) != hash {
		t.Fatal("preview defaulting changed parent reservation hash")
	}
	changed := parent.DeepCopy()
	changed.Spec.Previews = nil
	if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
		t.Fatalf("removed pinned preview config: %v", err)
	}
}
