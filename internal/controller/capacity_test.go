package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type syntheticCollector struct {
	now     time.Time
	count   int32
	cpu     int64
	unready bool
}

func (c *syntheticCollector) Collect(context.Context, *fleet.CelldFleet) capacity.Observation {
	o := capacity.Observation{At: c.now, Complete: true}
	for i := range c.count {
		o.Samples = append(o.Samples, capacity.Sample{Identity: fmt.Sprint(i), Ready: !c.unready || i < c.count-1, CPU: c.cpu, MemoryMiB: 100, RuntimeAt: c.now, RuntimeReceived: c.now, MetricsAt: c.now, MetricsReceived: c.now, Window: 15 * time.Second})
	}
	return o
}
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
func advancePolicy(t *testing.T, r *Reconciler, f *fleet.CelldFleet, c *syntheticCollector, n int) {
	t.Helper()
	for range n {
		reconcile(t, r, f)
		c.now = c.now.Add(15 * time.Second)
	}
}
func TestCapacityShadowAndAutomaticJournal(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = enableCapacity(t, r, f, "Shadow")
			c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 500}
			r.Collector = c
			r.now = func() time.Time { return c.now }
			before := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), before); err != nil {
				t.Fatal(err)
			}
			advancePolicy(t, r, f, c, 4)
			j := getJournal(t, r, f)
			if j.Applied != 3 || j.Operation != nil || j.Capacity.Decision.DesiredReplicas != 4 || len(j.History) != 0 {
				t.Fatal(j)
			}
			after := emptyObject(before)
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), after); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if !bytes.Equal(a, b) {
				t.Fatal("shadow mutated workload")
			}
			f = enableCapacity(t, r, f, "ScaleOut")
			advancePolicy(t, r, f, c, 3)
			j = getJournal(t, r, f)
			if j.Operation == nil || !j.Operation.Automatic || j.Operation.To != 4 {
				t.Fatal(j)
			}
			// Tightening bounds and switching to shadow does not abandon a recorded addition.
			f = enableCapacity(t, r, f, "Shadow")
			f.Spec.Capacity.MaxReplicas = 3
			if err := r.Update(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, Collector: c, now: func() time.Time { return c.now }}
			advancePolicy(t, r, f, c, 4)
			j = getJournal(t, r, f)
			if j.Applied != 4 || j.Operation != nil || len(j.History) != 1 {
				t.Fatal(j)
			}
			got := reconcile(t, r, f)
			if got.Spec.Replicas != 3 {
				t.Fatal("operator wrote manual field")
			}
			if j.Capacity.LastAction.IsZero() {
				t.Fatal("lost restart history")
			}
			// Slow or pressure-blocked joiners never produce a second addition.
			f = enableCapacity(t, r, f, "ScaleOut")
			c.count = 4
			c.unready = true
			c.now = c.now.Add(11 * time.Minute)
			advancePolicy(t, r, f, c, 3)
			j = getJournal(t, r, f)
			if j.Applied != 4 || j.Capacity.Decision.Reason != "IneffectiveCapacity" || len(j.History) != 1 {
				t.Fatal(j)
			}
		})
	}
}
func TestCapacityAutomaticContractionKeepsQualificationGates(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = enableCapacity(t, r, f, "Automatic")
			c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 10}
			r.Collector = c
			r.now = func() time.Time { return c.now }
			advancePolicy(t, r, f, c, 42)
			j := getJournal(t, r, f)
			if j.Applied != 3 || j.Operation == nil || j.Operation.Phase != "Blocked" || j.Capacity.Decision.DesiredReplicas != 2 {
				t.Fatal(j)
			}
			c.now = c.now.Add(15 * time.Second)
			got := reconcile(t, r, f)
			expected := "BucketCompletionUnqualified"
			if profile == "PersistentFleet" {
				expected = "FencingUnqualified"
			}
			reason(t, got, expected)
		})
	}
}
func TestCapacityManualOverrideAndRestart(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = enableCapacity(t, r, f, "ScaleOut")
	c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 500}
	r.Collector = c
	r.now = func() time.Time { return c.now }
	advancePolicy(t, r, f, c, 3)
	f = desiredCount(t, r, f, 5)
	advancePolicy(t, r, f, c, 6)
	j := getJournal(t, r, f)
	if j.Applied != 5 || len(j.History) != 1 {
		t.Fatal(j)
	}
	// A manual request persisted before a crash but before intent must not be lost.
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j.Capacity.ManualTarget = 6
	j.Capacity.LastManual = 6
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	f = desiredCount(t, r, f, 6)
	r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, Collector: c, now: func() time.Time { return c.now }}
	advancePolicy(t, r, f, c, 4)
	if getJournal(t, r, f).Applied != 6 {
		t.Fatal("manual override lost")
	}
}
func TestCapacityConflictingReconcilers(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = enableCapacity(t, r, f, "ScaleOut")
	c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 500}
	r.Collector = c
	r.now = func() time.Time { return c.now }
	advancePolicy(t, r, f, c, 2)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			other := &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, Collector: c, now: func() time.Time { return c.now }}
			_, _ = other.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
		})
	}
	wg.Wait()
	advancePolicy(t, r, f, c, 6)
	j := getJournal(t, r, f)
	if j.Applied != 4 || len(j.History) != 1 {
		t.Fatal(j)
	}
}
func TestCapacityRetainedClaimBlockCannotBeBypassed(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	f = enableCapacity(t, r, f, "ScaleOut")
	c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 500}
	r.Collector = c
	if err := r.Delete(t.Context(), &corev1.PersistentVolumeClaim{Name: f.Name + "-data-0", Namespace: f.Namespace}); err == nil {
		t.Fatal("unexpected claim name")
	}
	j := getJournal(t, r, f)
	for name := range j.Claims {
		if err := r.Delete(t.Context(), &corev1.PersistentVolumeClaim{Name: name, Namespace: f.Namespace}); err != nil {
			t.Fatal(err)
		}
		break
	}
	advancePolicy(t, r, f, c, 5)
	if j = getJournal(t, r, f); j.Operation != nil || j.Applied != 3 {
		t.Fatal(j)
	}
}

func TestCapacityQualifiedSeamRevalidatesBeforeRemoval(t *testing.T) {
	for _, change := range []string{"pressure", "policy", "loss", "allow"} {
		t.Run(change, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "PersistentFleet")
			f.Spec.Placement.AZCount = 1
			f.Spec.Placement.Zones = []string{"us-east-1a"}
			r := setup(t, f)
			r.Options.LocalTest = true
			e := &localEvidence{now: time.Unix(10000, 0), stopped: true}
			r.localLifecycle = e
			reconcile(t, r, f)
			reconcile(t, r, f)
			f = enableCapacity(t, r, f, "Automatic")
			c := &syntheticCollector{now: e.now, count: 3, cpu: 10}
			r.Collector = c
			r.now = func() time.Time { return c.now }
			advancePolicy(t, r, f, c, 41)
			j := getJournal(t, r, f)
			if j.Operation == nil || !j.Operation.Automatic || j.Operation.To != 2 {
				t.Fatal(j)
			}
			switch change {
			case "pressure":
				c.cpu = 500
			case "policy":
				f = enableCapacity(t, r, f, "Shadow")
			case "loss":
				e.loss = true
			}
			e.now = c.now
			got := reconcile(t, r, f)
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if change == "allow" {
				if replicas(w) != 2 {
					t.Fatal("qualified synthetic seam did not issue", got.Status)
				}
			} else {
				if replicas(w) != 3 {
					t.Fatal("capacity/recovery blocker bypassed", got.Status)
				}
				expected := map[string]string{"pressure": "CapacityUncertain", "policy": "CapacityChanged", "loss": "PossibleDataLoss"}[change]
				reason(t, got, expected)
				if change == "pressure" {
					c.cpu = 10
					c.now = c.now.Add(15 * time.Second)
					e.now = c.now
					got = reconcile(t, r, f)
					reason(t, got, "CapacityUncertain")
					if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
						t.Fatal(err)
					}
					if replicas(w) != 3 {
						t.Fatal("single low sample reused obsolete stabilization")
					}
				}
			}
		})
	}
}

func TestCapacityDisableDoesNotReplayOldManualTarget(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	j := getJournal(t, r, f)
	j.Capacity = &capacity.State{LastManual: 5, ManualTarget: 5, LastAction: time.Unix(1, 0)}
	target, automatic := r.capacityTarget(t.Context(), f, j)
	if target != 3 || automatic || j.Capacity.ManualTarget != 0 {
		t.Fatal(j)
	}
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Shadow"}
	f.Default()
	target, automatic = r.capacityTarget(t.Context(), f, j)
	if target != 3 || automatic || j.Capacity.LastAction.IsZero() {
		t.Fatal(j)
	}
}

type changingCollector struct {
	base   *syntheticCollector
	change func()
}

func (c changingCollector) Collect(ctx context.Context, f *fleet.CelldFleet) capacity.Observation {
	o := c.base.Collect(ctx, f)
	c.change()
	return o
}
func TestCapacityEditDuringCollectionDiscardsRecommendation(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = enableCapacity(t, r, f, "ScaleOut")
	c := &syntheticCollector{now: time.Unix(10000, 0), count: 3, cpu: 500}
	r.Collector = c
	r.now = func() time.Time { return c.now }
	advancePolicy(t, r, f, c, 2)
	r.Collector = changingCollector{base: c, change: func() { enableCapacity(t, r, f, "Shadow") }}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
	// The old status patch also correctly conflicts with the new spec revision.
	if err != nil && !apierrors.IsConflict(err) {
		t.Fatal(err)
	}
	r.Collector = c
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	if j.Operation != nil || j.Applied != 3 {
		t.Fatal("stale policy issued", j)
	}
}
