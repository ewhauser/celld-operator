package controller

import (
	"context"
	"fmt"
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

// podIdentities returns the capacity identities of the fleet's first n members.
func (x *operationFixture) podIdentities(n int) map[string]bool {
	x.t.Helper()
	ids := map[string]bool{}
	for i := range n {
		id, _ := podIdentity(x.pod(fmt.Sprintf("%s-%d", x.f.Name, i)))
		ids[id] = true
	}
	return ids
}

// An applied addition starts the scale-out cooldown, whether the policy or a
// manual edit asked for it: under sustained pressure the next policy addition
// waits scaleOutCooldownSeconds from the first write, reporting RateLimited.
// Only the policy's own addition is judged for redistribution.
func TestCapacityAdditionStartsScaleOutCooldown(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		manual     bool
	}{{"ScaleOut", "ScaleOut", false}, {"Automatic", "Automatic", false}, {"manual", "Automatic", true}} {
		t.Run(tc.name, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.f = enableCapacity(t, x.r, x.f, tc.mode)
			x.cpu = 1000
			// A complete observation of the three members precedes any edit.
			x.step()
			x.syncWorkload()
			x.clock = x.clock.Add(15 * time.Second)
			if tc.manual {
				x.desired(4)
			}
			var added time.Time
			limited := false
			for range 60 {
				f := x.step()
				n := replicas(x.workload())
				if added.IsZero() && n == 4 {
					added = x.clock
					s := x.state().Capacity
					if !s.LastAction.Equal(added) || (s.Addition != nil) == tc.manual {
						t.Fatalf("addition recorded as %v with %+v", s.LastAction, s.Addition)
					}
				}
				limited = limited || f.Status.Capacity.Reason == "RateLimited"
				if n == 5 {
					if cooldown := capacity.Seconds(x.f.Spec.Capacity.ScaleOutCooldownSeconds); x.clock.Sub(added) < cooldown {
						t.Fatalf("second addition %v after the first, within the %v cooldown", x.clock.Sub(added), cooldown)
					}
					if !limited {
						t.Fatal("the cooldown was never reported as RateLimited")
					}
					return
				}
				x.syncWorkload()
				x.clock = x.clock.Add(15 * time.Second)
			}
			t.Fatalf("sustained pressure never added a second member: %d replicas", replicas(x.workload()))
		})
	}
}

// An applied contraction starts the scale-in cooldown: under sustained low
// demand the next removal waits scaleInCooldownSeconds from the first,
// reporting RateLimited, even though its own low window completes sooner.
func TestCapacityContractionStartsScaleInCooldown(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.f = enableCapacity(t, x.r, x.f, "Automatic")
	var removed time.Time
	limited := false
	for range 150 {
		f := x.step()
		n := replicas(x.workload())
		if removed.IsZero() && n == 2 {
			removed = x.clock
			if s := x.state().Capacity; !s.LastAction.Equal(removed) {
				t.Fatalf("contraction recorded at %v, applied at %v", s.LastAction, removed)
			}
		}
		limited = limited || f.Status.Capacity.Reason == "RateLimited"
		if n == 1 {
			if cooldown := capacity.Seconds(x.f.Spec.Capacity.ScaleInCooldownSeconds); x.clock.Sub(removed) < cooldown {
				t.Fatalf("second removal %v after the first, within the %v cooldown", x.clock.Sub(removed), cooldown)
			}
			if !limited {
				t.Fatal("the cooldown was never reported as RateLimited")
			}
			return
		}
		x.syncWorkload()
		x.clock = x.clock.Add(15 * time.Second)
	}
	t.Fatalf("sustained low demand never removed a second member: %d replicas", replicas(x.workload()))
}

// A policy addition is judged against the observation that recommended it,
// of exactly the members it grew from. While the newcomer is observed, further
// additions hold at ObservingRedistribution; a newcomer that carries load
// releases the hold after redistributionObservationSeconds.
func TestCapacityAdditionObservesRedistribution(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.f = enableCapacity(t, x.r, x.f, "ScaleOut")
	x.cpu = 1000
	incumbents := x.podIdentities(3)
	for range 10 {
		x.step()
		if replicas(x.workload()) == 4 {
			break
		}
		x.syncWorkload()
		x.clock = x.clock.Add(15 * time.Second)
	}
	a := x.state().Capacity.Addition
	if replicas(x.workload()) != 4 || a == nil || a.Target != 4 || len(a.Before) != 3 {
		t.Fatalf("the policy addition is not observed for redistribution: %+v", a)
	}
	for id := range a.Before {
		if !incumbents[id] {
			t.Fatalf("redistribution baseline %s is not a pre-change member", id)
		}
	}
	var since time.Time
	for range 20 {
		x.syncWorkload()
		x.clock = x.clock.Add(15 * time.Second)
		f := x.step()
		if x.state().Capacity.Addition == nil {
			if window := capacity.Seconds(x.f.Spec.Capacity.RedistributionObservationSeconds); since.IsZero() || x.clock.Sub(since) < window {
				t.Fatalf("released after %v of observation, before the %v window", x.clock.Sub(since), window)
			}
			return
		}
		if reason := f.Status.Capacity.Reason; reason != "ObservingRedistribution" || replicas(x.workload()) != 4 {
			t.Fatalf("while the newcomer is observed: %s at %d replicas", reason, replicas(x.workload()))
		}
		if since.IsZero() {
			since = x.clock
		}
	}
	t.Fatal("a newcomer carrying load never released the redistribution hold")
}

// Additions that leave the incumbents hot while each newcomer idles are
// ineffective. After two such batches the policy holds at LoadNotRedistributed
// rather than growing to its maximum.
func TestCapacityHoldsIneffectiveAdditions(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.f = enableCapacity(t, x.r, x.f, "ScaleOut")
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Capacity.MaxReplicas = 10 })
	x.cpu = 1000
	hot := x.podIdentities(3)
	collect := x.r.Collector
	x.r.Collector = collectorFunc(func(ctx context.Context, f *fleet.CelldFleet) capacity.Observation {
		o := collect.Collect(ctx, f)
		for i := range o.Samples {
			if !hot[o.Samples[i].Identity] {
				o.Samples[i].CPU = 0
			}
		}
		return o
	})
	for range 80 {
		x.step()
		x.syncWorkload()
		x.clock = x.clock.Add(15 * time.Second)
	}
	f := x.step()
	if n := replicas(x.workload()); n != 5 || f.Status.Capacity.Reason != "LoadNotRedistributed" || x.state().Capacity.IneffectiveBatches != 2 {
		t.Fatalf("idle additions grew the fleet to %d: %+v", n, f.Status.Capacity)
	}
}

// Each step the operator writes under a built-in policy is recorded once, by
// the reconcile that writes it; reconciles in which a step waits for the
// previous one to roll out record nothing. External mode records nothing: the
// /scale writer paces its own requests.
func TestCapacityRecordsEachWrittenStepOnce(t *testing.T) {
	for _, mode := range []string{"Shadow", "ScaleOut", "Automatic", "External"} {
		t.Run(mode, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.f = enableCapacity(t, x.r, x.f, mode)
			recorded := func(want time.Time) {
				t.Helper()
				if mode == "External" {
					want = time.Time{}
				}
				if got := x.state().Capacity.LastAction; !got.Equal(want) {
					t.Fatalf("last action %v, want %v", got, want)
				}
			}
			x.desired(1)
			x.step()
			first := x.clock
			for range 3 {
				x.clock = x.clock.Add(15 * time.Second)
				x.step()
			}
			if replicas(x.workload()) != 2 {
				t.Fatalf("the second step did not wait for the first to roll out: %d replicas", replicas(x.workload()))
			}
			recorded(first)
			x.syncWorkload()
			x.clock = x.clock.Add(15 * time.Second)
			x.step()
			if replicas(x.workload()) != 1 {
				t.Fatalf("the second step was not applied: %d replicas", replicas(x.workload()))
			}
			recorded(x.clock)
		})
	}
}
