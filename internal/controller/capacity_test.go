package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/launcher"
	corev1 "k8s.io/api/core/v1"
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
func TestCapacityEntriesUseStrictCurrentOperation(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, mode := range []string{"Automatic", "External", "Shadow", "ScaleOut"} {
			t.Run(profile+"/"+mode, func(t *testing.T) {
				x := newOperationFixture(t, profile)
				x.f = enableCapacity(t, x.r, x.f, mode)
				if mode == "External" {
					x.desired(2)
				}
				for range 50 {
					x.step()
					if x.state().Operation != nil {
						break
					}
					x.clock = x.clock.Add(15 * time.Second)
				}
				if mode == "Shadow" || mode == "ScaleOut" {
					if x.state().Operation != nil || x.requests != 0 {
						t.Fatal("read-only policy contracted")
					}
					return
				}
				o := x.state().Operation
				if o == nil || o.Kind != "Scale" || o.To != 2 || len(o.Targets) != 1 {
					t.Fatal("capacity bypassed strict executor", o)
				}
				x.until("Requesting")
				x.r.launcherCall = func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error) {
					return launcher.State{}, errors.New("lost launcher")
				}
				for range 3 {
					x.step()
				}
				if replicas(x.workload()) != 3 {
					t.Fatal("automatic removal accepted missing launcher proof")
				}
			})
		}
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
	if x.state().Operation != nil || x.requests != 0 || replicas(x.workload()) != 3 {
		t.Fatal("request changed during collection but was issued")
	}
	y := newOperationFixture(t, "PersistentFleet")
	y.desired(2)
	y.until("Intent")
	y.cpu = 10000
	for range 3 {
		y.step()
	}
	if y.requests != 0 || y.state().Operation.Phase != "Intent" {
		t.Fatal("unsafe survivor capacity issued")
	}
}

type collectorFunc func(context.Context, *fleet.CelldFleet) capacity.Observation

func (f collectorFunc) Collect(ctx context.Context, subject *fleet.CelldFleet) capacity.Observation {
	return f(ctx, subject)
}
