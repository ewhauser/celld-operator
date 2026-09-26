package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// tick lets simulated Kubernetes act, advances one sample interval and
// reconciles.
func (x *operationFixture) tick() *fleet.CelldFleet {
	x.t.Helper()
	x.syncWorkload()
	x.clock = x.clock.Add(15 * time.Second)
	return x.step()
}

// awaitReplicas reconciles, one sample interval apart, until the controller
// writes n replicas, and returns the time of that write.
func (x *operationFixture) awaitReplicas(n int32) time.Time {
	x.t.Helper()
	x.step()
	for range 60 {
		if replicas(x.workload()) == n {
			return x.clock
		}
		x.tick()
	}
	x.t.Fatalf("the controller never wrote %d replicas: %d", n, replicas(x.workload()))
	return time.Time{}
}

// restartContainer simulates the kubelet restarting a member's runtime
// container, which gives the member a new capacity identity.
func (x *operationFixture) restartContainer(name string) {
	x.t.Helper()
	p := x.pod(name)
	c := &p.Status.ContainerStatuses[0]
	c.RestartCount++
	c.ContainerID = fmt.Sprintf("%s-%d", c.ContainerID, c.RestartCount)
	c.State.Running.StartedAt = metav1.NewTime(x.clock)
	if err := x.r.Status().Update(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
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

// An addition is judged against the members it grew from, by container
// identity. An incumbent that restarts or is replaced before the addition is
// judged never returns under that identity, so no later observation can judge
// the addition. It is dropped, counting as neither relief nor an ineffective
// batch, and sustained pressure adds the next member after a new stable window
// and the scale-out cooldown instead of holding the fleet for good.
func TestCapacityIncumbentRestartDropsAddition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart func(*operationFixture)
	}{
		{"container restarted", func(x *operationFixture) { x.restartContainer(x.f.Name + "-0") }},
		{"pod replaced", func(x *operationFixture) {
			if err := x.r.Delete(x.t.Context(), x.pod(x.f.Name+"-0")); err != nil {
				x.t.Fatal(err)
			}
			x.syncWorkload()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.f = enableCapacity(t, x.r, x.f, "ScaleOut")
			x.cpu = 1000
			added := x.awaitReplicas(4)
			if f := x.tick(); f.Status.Capacity.Reason != "ObservingRedistribution" {
				t.Fatalf("the newcomer is not under observation: %+v", f.Status.Capacity)
			}
			tc.restart(x)
			f := x.tick()
			if s := x.state().Capacity; s.Addition != nil || s.IneffectiveBatches != 0 || f.Status.Capacity.Reason != "StabilizingOut" {
				t.Fatalf("an addition that can no longer be judged was kept or counted: %+v, %d ineffective, %+v", s.Addition, s.IneffectiveBatches, f.Status.Capacity)
			}
			for range 40 {
				if replicas(x.workload()) == 5 {
					if cooldown := capacity.Seconds(x.f.Spec.Capacity.ScaleOutCooldownSeconds); x.clock.Sub(added) < cooldown {
						t.Fatalf("next addition %v after the first, within the %v cooldown", x.clock.Sub(added), cooldown)
					}
					return
				}
				x.tick()
			}
			t.Fatalf("sustained pressure never added a member after the restart: %d replicas, %+v", replicas(x.workload()), x.state().Capacity.Decision)
		})
	}
}

// A contraction below an addition's target before the addition is judged
// removes members the judgment needs, so no later observation can judge it.
// Whether a manual edit or an External /scale writer contracts the fleet, the
// addition is dropped instead of holding every later addition at
// ObservingRedistribution, and sustained pressure adds a member again after a
// new stable window and the scale-out cooldown.
func TestCapacityContractionDropsAddition(t *testing.T) {
	for _, source := range []string{"manual", "External"} {
		t.Run(source, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.f = enableCapacity(t, x.r, x.f, "ScaleOut")
			x.cpu = 1000
			x.awaitReplicas(4)
			if f := x.tick(); f.Status.Capacity.Reason != "ObservingRedistribution" {
				t.Fatalf("the newcomer is not under observation: %+v", f.Status.Capacity)
			}
			below := int32(2)
			if source == "manual" {
				x.desired(below)
			} else {
				// The /scale writer asks for three members. Its contraction
				// shares Automatic's survivor gate, so demand drops first.
				below = 3
				x.cpu = 10
				x.edit(func(f *fleet.CelldFleet) { f.Spec.Capacity.Mode, f.Spec.Replicas = "External", below })
			}
			for range 10 {
				if replicas(x.workload()) == below {
					break
				}
				x.tick()
			}
			if n := replicas(x.workload()); n != below {
				t.Fatalf("the fleet did not contract to %d: %d", below, n)
			}
			if source == "External" {
				x.edit(func(f *fleet.CelldFleet) { f.Spec.Capacity.Mode = "ScaleOut" })
				x.cpu = 1000
			}
			s := x.state().Capacity
			if s.Addition == nil || s.Addition.Target != 4 {
				t.Fatalf("no addition was under observation when the fleet contracted: %+v", s.Addition)
			}
			last := s.LastAction
			for range 60 {
				f := x.tick()
				if replicas(x.workload()) > below {
					if cooldown := capacity.Seconds(x.f.Spec.Capacity.ScaleOutCooldownSeconds); x.clock.Sub(last) < cooldown {
						t.Fatalf("addition %v after the last action, within the %v cooldown", x.clock.Sub(last), cooldown)
					}
					if b := x.state().Capacity.IneffectiveBatches; b != 0 {
						t.Fatalf("the dropped addition counted as %d ineffective batches", b)
					}
					return
				}
				if reason := f.Status.Capacity.Reason; reason == "ObservingRedistribution" {
					t.Fatalf("held at %s with %d members, below the observed addition", reason, replicas(x.workload()))
				}
			}
			t.Fatalf("sustained pressure never added a member after the contraction: %d replicas, %+v", replicas(x.workload()), x.state().Capacity.Decision)
		})
	}
}

// An addition that can no longer be judged counts as neither relief nor an
// ineffective batch. After two ineffective additions hold the fleet at
// LoadNotRedistributed, an incumbent restart drops the held addition. Under
// sustained pressure the policy then adds one member, judges it like any other
// and holds again, rather than holding for good or growing to its maximum.
func TestCapacityUnjudgedAdditionKeepsIneffectiveBatches(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.f = enableCapacity(t, x.r, x.f, "ScaleOut")
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Capacity.MaxReplicas = 10 })
	x.cpu = 1000
	// The first three members stay hot; every newcomer idles.
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
	held := func(f *fleet.CelldFleet) bool {
		return f.Status.Capacity.Reason == "LoadNotRedistributed" && x.state().Capacity.IneffectiveBatches == 2
	}
	f := x.step()
	for range 80 {
		if held(f) {
			break
		}
		f = x.tick()
	}
	if n := replicas(x.workload()); n != 5 || !held(f) {
		t.Fatalf("two idle additions did not hold the fleet: %d replicas, %+v", n, f.Status.Capacity)
	}
	x.restartContainer(x.f.Name + "-1")
	// The restarted member keeps its disk and cells, so it stays hot.
	restarted, _ := podIdentity(x.pod(x.f.Name + "-1"))
	hot[restarted] = true
	for range 240 {
		f = x.tick()
		if b := x.state().Capacity.IneffectiveBatches; b != 2 {
			t.Fatalf("the unjudged addition changed the ineffective count to %d", b)
		}
	}
	if n := replicas(x.workload()); n != 6 || !held(f) {
		t.Fatalf("an hour after the restart: %d replicas, %+v", n, f.Status.Capacity)
	}
}
