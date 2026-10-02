package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPreviewPoolExistingReservationDoesNotWrite(t *testing.T) {
	pool := previewPoolFixture(previewFixture())
	base := fake.NewClientBuilder().WithScheme(envtestScheme(t)).Build()
	if err := reservePreviewPool(t.Context(), base, pool); err != nil {
		t.Fatal(err)
	}
	writes := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		writes++
		return c.Create(ctx, obj, opts...)
	}})
	if err := reservePreviewPool(t.Context(), c, pool); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("existing pool reservation issued %d create requests", writes)
	}
}

func TestApplicationMembershipRejectsDuplicatePods(t *testing.T) {
	before := []corev1.Pod{{Name: "one", UID: "one"}, {Name: "two", UID: "two"}}
	after := []corev1.Pod{before[0], before[0]}
	if sameApplicationPods(before, after) {
		t.Fatal("duplicate closing membership hid a missing Pod")
	}
}

func TestPreviewPoolReservationCreateRace(t *testing.T) {
	for _, sameOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameOwner=%t", sameOwner), func(t *testing.T) {
			pool := previewPoolFixture(previewFixture())
			base := fake.NewClientBuilder().WithScheme(envtestScheme(t)).Build()
			winner := pool.DeepCopy()
			if !sameOwner {
				winner.UID = "other-owner"
			}
			if err := reservePreviewPool(t.Context(), base, winner); err != nil {
				t.Fatal(err)
			}
			reads := 0
			c := interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				if reads == 1 {
					return apierrors.NewNotFound(schema.GroupResource{Group: fleet.GroupVersion.Group, Resource: "celldstoragereservations"}, key.Name)
				}
				if reservation := obj.(*fleet.CelldStorageReservation); reservation.Spec != (fleet.ReservationSpec{}) {
					t.Fatal("winner read reused the proposed reservation's fields")
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			err := reservePreviewPool(t.Context(), c, pool)
			if (err == nil) != sameOwner {
				t.Fatalf("reservation race ownership result: %v", err)
			}
			if reads != 2 {
				t.Fatalf("reservation race performed %d reads", reads)
			}
		})
	}
}

func BenchmarkPreviewSeedReceipt(b *testing.B) {
	for _, size := range []int{1, 100} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			f := &fleet.CelldFleet{UID: "target"}
			s := &fleet.CelldStorageReservation{
				Spec:   fleet.ReservationSpec{Initialization: &fleet.PreviewSeedRequest{Target: fleet.PreviewSeedTarget{PreviewUID: "preview"}, Selection: fleet.PreviewSeedSpec{Source: "source"}}},
				Status: fleet.PreviewSeedStatus{Phase: "Succeeded", ExecutorID: "executor", TargetFleetUID: string(f.UID), Manifest: &fleet.PreviewSnapshotManifest{}},
			}
			for i := range size {
				object := fleet.PreviewObjectReference{Class: "Account", ID: fmt.Sprintf("object-%03d", i)}
				s.Spec.Initialization.Selection.Objects = append(s.Spec.Initialization.Selection.Objects, object)
				s.Status.Manifest.Objects = append(s.Status.Manifest.Objects, fleet.PreviewObjectSnapshot{PreviewObjectReference: object, SnapshotID: "snapshot", SourceVersion: "v1", Digest: strings.Repeat("a", 64)})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := completedSeedReceipt(s, f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkApplicationMembership(b *testing.B) {
	for _, size := range []int{3, 100} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			pods := make([]corev1.Pod, size)
			for i := range pods {
				pods[i] = corev1.Pod{Name: fmt.Sprintf("pod-%03d", i), UID: types.UID(fmt.Sprint(i)), Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}, OwnerReferences: []metav1.OwnerReference{{UID: "workload", Kind: "StatefulSet"}}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if !sameApplicationPods(pods, pods) {
					b.Fatal("unchanged membership differs")
				}
			}
		})
	}
}

func BenchmarkPreviewPoolReservation(b *testing.B) {
	scheme := runtime.NewScheme()
	if err := fleet.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	pool := previewPoolFixture(previewFixture())
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := reservePreviewPool(b.Context(), base, pool); err != nil {
		b.Fatal(err)
	}
	creates, gets := 0, 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates++
			return c.Create(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets++
			return c.Get(ctx, key, obj, opts...)
		},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := reservePreviewPool(b.Context(), c, pool); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(creates)/float64(b.N), "creates/op")
	b.ReportMetric(float64(gets)/float64(b.N), "gets/op")
}

func BenchmarkRoutingReadiness(b *testing.B) {
	f := fixture("application", "bucket", "Bucket")
	f.Spec.Routing = testRouting()
	f.Spec.Routing.Ingress = nil
	f.Spec.Routing.Gateway = &fleet.GatewayRouting{Name: "gateway", Namespace: "edge"}
	route := routingObject(f, routeGVK)
	route.SetGeneration(1)
	parents := make([]any, 32)
	for i := range parents {
		name := fmt.Sprintf("other-%d", i)
		if i == len(parents)-1 {
			name = "gateway"
		}
		parents[i] = map[string]any{
			"parentRef": map[string]any{"name": name, "namespace": "edge"},
			"conditions": []any{
				map[string]any{"type": "Accepted", "status": "True", "observedGeneration": int64(1)},
				map[string]any{"type": "ResolvedRefs", "status": "True", "observedGeneration": int64(1)},
			},
		}
	}
	if err := unstructured.SetNestedSlice(route.Object, parents, "status", "parents"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if status, _, _ := routingReadiness(f, route); status != metav1.ConditionTrue {
			b.Fatal("accepted route is not ready")
		}
	}
}
