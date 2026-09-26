package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func enableCapacity(t *testing.T, r *Reconciler, f *fleet.CelldFleet, mode string) *fleet.CelldFleet {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Capacity = &fleet.CapacityPolicy{Mode: mode, MinReplicas: 1, MaxReplicas: 5}
	got.Default()
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Every capacity entry point contracts a PersistentFleet one member at a time,
// and never while a rollout is in progress.
func TestCapacityEntriesContractOneMemberAfterRollout(t *testing.T) {
	for _, mode := range []string{"Automatic", "External", "Shadow", "ScaleOut"} {
		t.Run(mode, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.f = enableCapacity(t, x.r, x.f, mode)
			if mode == "External" {
				x.desired(2)
			}
			x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"} })
			x.hold = true
			for range 20 {
				x.step()
				x.syncWorkload()
				x.clock = x.clock.Add(15 * time.Second)
			}
			if replicas(x.workload()) != 3 {
				t.Fatal("contracted while a rollout was in progress")
			}
			x.hold = false
			for range 50 {
				x.step()
				x.syncWorkload()
				if replicas(x.workload()) != 3 {
					break
				}
				x.clock = x.clock.Add(15 * time.Second)
			}
			if mode == "Shadow" || mode == "ScaleOut" {
				if replicas(x.workload()) != 3 {
					t.Fatal("read-only policy contracted")
				}
				return
			}
			if replicas(x.workload()) != 2 {
				t.Fatalf("capacity did not remove one member: %d", replicas(x.workload()))
			}
		})
	}
}
func TestCapacityCollectionEditAndRevalidation(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.f = enableCapacity(t, x.r, x.f, "Automatic")
	x.cpu = 1000
	original := x.r.Collector
	base := x.r.Client
	x.r.Collector = collectorFunc(func(ctx context.Context, f *fleet.CelldFleet) capacity.Observation {
		observation := original.Collect(ctx, f)
		changed := &fleet.CelldFleet{}
		if err := base.Get(ctx, client.ObjectKeyFromObject(f), changed); err != nil {
			t.Fatal(err)
		}
		changed.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
		if err := base.Update(ctx, changed); err != nil {
			t.Fatal(err)
		}
		return observation
	})
	for range 5 {
		_, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)})
		if err != nil && !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
		x.clock = x.clock.Add(15 * time.Second)
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("request changed during collection but was applied")
	}
}

type collectorFunc func(context.Context, *fleet.CelldFleet) capacity.Observation

func (f collectorFunc) Collect(ctx context.Context, subject *fleet.CelldFleet) capacity.Observation {
	return f(ctx, subject)
}
