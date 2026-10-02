package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestPreviewPoolIndexedFanout(t *testing.T) {
	p := previewFixture()
	otherPool := p.DeepCopy()
	otherPool.Name = "other-pool-preview"
	otherPool.Spec.FleetRef.Name = "another-pool"
	otherNamespace := p.DeepCopy()
	otherNamespace.Namespace = "other-namespace"
	c := fake.NewClientBuilder().WithScheme(envtestScheme(t)).WithObjects(p, otherPool, otherNamespace).
		WithIndex(&fleet.CelldPreview{}, previewFleetRefIndex, previewFleetRefValues).Build()
	requests := previewPoolRequests(t.Context(), c, previewPoolFixture(p))
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(p) {
		t.Fatalf("unexpected pool fanout: %+v", requests)
	}
}

func TestEnvtestPreviewPoolCachedIndex(t *testing.T) {
	direct := envtestClient(t)
	mgr, err := ctrl.NewManager(envtestConfig, ctrl.Options{Scheme: envtestScheme(t), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&PreviewReconciler{Client: direct}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	p := previewFixture()
	p.Namespace = fmt.Sprintf("preview-index-%d", envtestSeq.Add(1))
	p.UID = ""
	p.CreationTimestamp = metav1.Time{}
	if err := direct.Create(t.Context(), &corev1.Namespace{Name: p.Namespace}); err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	other := p.DeepCopy()
	other.Name = "other-pool-preview"
	other.UID = ""
	other.ResourceVersion = ""
	other.Spec.FleetRef.Name = "another-pool"
	if err := direct.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mgr.GetCache().Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("preview cache did not sync")
	}
	requests := previewPoolRequests(ctx, mgr.GetClient(), previewPoolFixture(p))
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(p) {
		t.Fatalf("cache field index returned wrong pool: %+v", requests)
	}
	// Deletion must remove the cache's field-index entry independently of direct
	// reconciliation reads.
	if err := direct.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(previewPoolRequests(ctx, mgr.GetClient(), previewPoolFixture(p))) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("deleted preview remained in pool index")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// previewFanoutReader models the cache's indexed lookup and DeepCopy result
// materialization with a real client-go Indexer, without network latency. The
// production cache is verified separately against envtest above.
type previewFanoutReader struct {
	client.Reader
	indexer cache.Indexer
}

func (r previewFanoutReader) List(_ context.Context, list client.ObjectList, options ...client.ListOption) error {
	opts := (&client.ListOptions{}).ApplyOptions(options)
	indexName, key := cache.NamespaceIndex, opts.Namespace
	if opts.FieldSelector != nil {
		if name, ok := opts.FieldSelector.RequiresExactMatch(previewFleetRefIndex); ok {
			indexName, key = previewFleetRefIndex, opts.Namespace+"/"+name
		}
	}
	objects, err := r.indexer.ByIndex(indexName, key)
	if err != nil {
		return err
	}
	previews := list.(*fleet.CelldPreviewList)
	previews.Items = make([]fleet.CelldPreview, len(objects))
	for i, object := range objects {
		object.(*fleet.CelldPreview).DeepCopyInto(&previews.Items[i])
	}
	return nil
}

func BenchmarkPreviewPoolFanout(b *testing.B) {
	for _, size := range []int{100, 10000} {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			previewFleetRefIndex: func(object any) ([]string, error) {
				p := object.(*fleet.CelldPreview)
				return []string{p.Namespace + "/" + p.Spec.FleetRef.Name}, nil
			},
		})
		p := previewFixture()
		pool := previewPoolFixture(p)
		for i := range size {
			object := p.DeepCopy()
			object.Name = fmt.Sprintf("preview-%d", i)
			if i >= 10 {
				object.Spec.FleetRef.Name = "other-pool"
			}
			if err := indexer.Add(object); err != nil {
				b.Fatal(err)
			}
		}
		reader := previewFanoutReader{indexer: indexer}
		for _, indexed := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/indexed=%t", size, indexed), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var requests []ctrl.Request
					if indexed {
						requests = previewPoolRequests(b.Context(), reader, pool)
					} else {
						var previews fleet.CelldPreviewList
						if err := reader.List(b.Context(), &previews, client.InNamespace(pool.Namespace)); err != nil {
							b.Fatal(err)
						}
						for _, object := range previews.Items {
							if object.Spec.FleetRef.Name == pool.Name {
								requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&object)})
							}
						}
					}
					if len(requests) != 10 {
						b.Fatalf("unexpected request count: %d", len(requests))
					}
				}
			})
		}
	}
}

func TestRoutingReadinessLeavesStatusUntouched(t *testing.T) {
	f := fixture("application", "bucket", "Bucket")
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "gateway"}
	route := routingObject(f, routeGVK)
	route.Object["status"] = map[string]any{"parents": []any{map[string]any{"parentRef": map[string]any{"name": "gateway"}}}}
	before := route.DeepCopy()
	routingReadiness(f, route)
	if !equality.Semantic.DeepEqual(before.Object, route.Object) {
		t.Fatal("routing readiness mutated its cached status")
	}
}

func TestRoutingParentMatchingDefaultsAndIdentity(t *testing.T) {
	f := fixture("application", "bucket", "Bucket")
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "gateway"}
	wanted := routeParent(f)
	for _, test := range []struct {
		name string
		ref  map[string]any
		want bool
	}{
		{name: "defaulted", ref: map[string]any{"name": "gateway"}, want: true},
		{name: "explicit", ref: wanted, want: true},
		{name: "different namespace", ref: map[string]any{"name": "gateway", "namespace": "other"}},
		{name: "different group", ref: map[string]any{"name": "gateway", "group": "other"}},
		{name: "different kind", ref: map[string]any{"name": "gateway", "kind": "Service"}},
		{name: "different name", ref: map[string]any{"name": "other"}},
		{name: "unexpected port", ref: map[string]any{"name": "gateway", "port": int64(443)}},
		{name: "empty section", ref: map[string]any{"name": "gateway", "sectionName": ""}},
		{name: "invalid name type", ref: map[string]any{"name": []any{"gateway"}}},
		{name: "null group", ref: map[string]any{"name": "gateway", "group": nil}},
		{name: "absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := routingParentMatches(wanted, test.ref, f.Namespace); got != test.want {
				t.Fatalf("parent match = %t, want %t", got, test.want)
			}
		})
	}
}
