package controller

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMatchesDoesNotMutateObjects(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		f := fixture("alpha", "bucket-alpha", profile)
		objects := append(prerequisites(f, Options{OperatorNamespace: "operator"}), workload(f, Options{}))
		for _, want := range objects {
			t.Run(fmt.Sprintf("%s/%T/%s", profile, want, want.GetName()), func(t *testing.T) {
				got := want.DeepCopyObject().(client.Object)
				wantBefore, gotBefore := want.DeepCopyObject(), got.DeepCopyObject()
				if !matches(want, got) {
					t.Fatal("identical resource did not match")
				}
				if !reflect.DeepEqual(wantBefore, want) || !reflect.DeepEqual(gotBefore, got) {
					t.Fatal("normalization mutated an input object")
				}
			})
		}
	}
}

func BenchmarkResourceMatches(b *testing.B) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	objects := append(prerequisites(f, Options{OperatorNamespace: "operator"}), workload(f, Options{}))
	for _, obj := range objects {
		b.Run(fmt.Sprintf("%T/%s", obj, obj.GetName()), func(b *testing.B) {
			got := obj.DeepCopyObject().(client.Object)
			got.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "celld-operator", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:replicas":{},"f:template":{"f:spec":{"f:containers":{},"f:affinity":{}}}}}`)}})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if !matches(obj, got) {
					b.Fatal("resource did not match")
				}
			}
		})
	}
}

func BenchmarkResources(b *testing.B) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	b.ReportAllocs()
	for b.Loop() {
		_ = workload(f, Options{})
		_ = prerequisites(f, Options{OperatorNamespace: "operator"})
	}
}

func BenchmarkPublishFleetMetrics(b *testing.B) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Capacity = &fleet.CapacityPolicy{}
	f.Default()
	f.Status.Capacity = fleet.CapacityStatus{Mode: "Shadow", Reason: "WithinThresholds", DesiredReplicas: 3, UsefulReplicas: 3, CoveredReplicas: 3}
	f.Status.DesiredReplicas = 3
	f.Status.BlockedSince = "2026-01-01T00:00:00Z"
	now := time.Unix(1767225605, 0)
	defer clearFleetMetrics(f.Namespace, f.Name)
	publishFleetMetrics(f, stateFootprint{}, now)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		publishFleetMetrics(f, stateFootprint{}, now)
	}
}
