package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type applicationReaderFunc func(context.Context, controlplane.Target) (controlplane.Application, error)

func (f applicationReaderFunc) Application(ctx context.Context, target controlplane.Target) (controlplane.Application, error) {
	return f(ctx, target)
}
func applicationSnapshot(now time.Time) controlplane.Application {
	v := controlplane.ApplicationVersion{Version: "v2", Prefix: "deploy/v2/"}
	return controlplane.Application{RuntimeGeneration: "process-a", SampledAtMS: now.UnixMilli(), SnapshotValid: true, Loaded: &v, LocalGeneration: 2, Target: &controlplane.ApplicationTarget{ApplicationVersion: v, ObservedAtMS: now.UnixMilli()}, PointerStatus: "observed", AdoptionStatus: "adopted", ResidentCells: 3, ReceivedAt: now}
}
func TestApplicationConvergence(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet", "Deployment"} {
		for _, scenario := range []string{"converged", "mixed versions", "delayed cells", "swapping", "failed adoption", "stale target", "unreachable", "unsupported", "target disagreement", "same version different artifacts", "rollback"} {
			t.Run(profile+"/"+scenario, func(t *testing.T) {
				x := newOperationFixture(t, profile)
				x.r.ApplicationRuntime = applicationReaderFunc(func(_ context.Context, target controlplane.Target) (controlplane.Application, error) {
					a := applicationSnapshot(x.clock)
					if scenario == "rollback" {
						a.Loaded.Version = "v1"
						a.Loaded.Prefix = "deploy/v1/"
						a.Target.ApplicationVersion = *a.Loaded
					}
					if target.IP == "10.0.0.3" {
						switch scenario {
						case "mixed versions":
							a.Loaded.Version = "v1"
						case "delayed cells":
							a.PendingCells = 1
						case "swapping":
							a.SwappingCells = 1
						case "failed adoption":
							a.AdoptionStatus = "failed"
						case "stale target":
							a.Target.ObservedAtMS = x.clock.Add(-2 * time.Minute).UnixMilli()
						case "unreachable":
							return a, errors.New("unreachable")
						case "unsupported":
							return a, controlplane.ErrUnsupported
						case "target disagreement":
							a.Target.Version = "v3"
						case "same version different artifacts":
							a.Loaded.Prefix = "other/"
						}
					}
					return a, nil
				})
				got, status, reason := x.r.observeApplication(t.Context(), x.f)
				want := metav1.ConditionFalse
				switch scenario {
				case "converged", "rollback":
					want = metav1.ConditionTrue
				case "stale target", "unreachable", "unsupported", "target disagreement":
					want = metav1.ConditionUnknown
				}
				if status != want {
					t.Fatalf("want %s, got %s (%s): %+v", want, status, reason, got)
				}
				if got.ExpectedNodes != 3 || len(got.Nodes) != 3 {
					t.Fatalf("wrong coverage: %+v", got)
				}
				if want == metav1.ConditionUnknown && got.Target != nil {
					t.Fatal("published authoritative target with incomplete observations")
				}
				if scenario == "delayed cells" && got.PendingCells != 1 {
					t.Fatal("pending cells lost")
				}
				if scenario == "mixed versions" && len(got.Versions) != 2 {
					t.Fatal("mixed versions lost")
				}
			})
		}
	}
}
func TestApplicationMembershipRace(t *testing.T) {
	for _, scenario := range []string{"restart", "replace", "new pod", "terminating", "foreign owner", "readiness"} {
		t.Run(scenario, func(t *testing.T) {
			x := newOperationFixture(t, "Bucket")
			var once sync.Once
			x.r.ApplicationRuntime = applicationReaderFunc(func(ctx context.Context, _ controlplane.Target) (controlplane.Application, error) {
				once.Do(func() {
					p := &corev1.Pod{}
					if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "alpha-0"}, p); err != nil {
						t.Error(err)
						return
					}
					switch scenario {
					case "restart":
						p.Status.ContainerStatuses[0].RestartCount++
						if err := x.r.Status().Update(ctx, p); err != nil {
							t.Error(err)
						}
					case "replace":
						if err := x.r.Delete(ctx, p); err != nil {
							t.Error(err)
						}
						p.ResourceVersion = ""
						p.UID = "replacement"
						if err := x.r.Create(ctx, p); err != nil {
							t.Error(err)
						}
					case "new pod":
						p.Name = "alpha-extra"
						p.UID = "new-pod"
						p.ResourceVersion = ""
						if err := x.r.Create(ctx, p); err != nil {
							t.Error(err)
						}
					case "terminating":
						p.Finalizers = []string{"test/hold"}
						if err := x.r.Update(ctx, p); err != nil {
							t.Error(err)
						}
						if err := x.r.Delete(ctx, p); err != nil {
							t.Error(err)
						}
					case "foreign owner":
						p.OwnerReferences[0].UID = "foreign"
						if err := x.r.Update(ctx, p); err != nil {
							t.Error(err)
						}
					case "readiness":
						p.Status.Conditions = nil
						if err := x.r.Status().Update(ctx, p); err != nil {
							t.Error(err)
						}
					}
				})
				return applicationSnapshot(x.clock), nil
			})
			_, status, _ := x.r.observeApplication(t.Context(), x.f)
			if status != metav1.ConditionUnknown {
				t.Fatal("membership change preserved convergence")
			}
		})
	}
}
func TestApplicationObservationCannotBlockIssuedRemoval(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	x.until("Requesting")
	id := x.state().Operation.ID
	x.r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		return controlplane.Application{}, errors.New("offline")
	})
	x.finish()
	if s := x.state(); s.Applied != 2 || s.Completion.ID != id {
		t.Fatal("application observation stalled removal")
	}
	f := reconcile(t, x.r, x.f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "ApplicationConverged"); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatalf("missing observation condition: %+v", c)
	}
}
func TestApplicationStatusPatchAndPollInterval(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		return applicationSnapshot(x.clock), nil
	})
	f := reconcile(t, x.r, x.f)
	before := f.Status.Application
	if c := meta.FindStatusCondition(f.Status.Conditions, "ApplicationConverged"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("missing convergence: %+v", c)
	}
	x.r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		t.Error("polled too soon")
		return controlplane.Application{}, nil
	})
	f = reconcile(t, x.r, f)
	if !f.Status.Application.ObservedAt.Equal(&before.ObservedAt) {
		t.Fatal("rewrote observation before poll interval")
	}
	x.clock = x.clock.Add(20 * time.Second)
	x.r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		return controlplane.Application{}, controlplane.ErrUnsupported
	})
	f = reconcile(t, x.r, f)
	if c := meta.FindStatusCondition(f.Status.Conditions, "ApplicationConverged"); c.Status != metav1.ConditionUnknown {
		t.Fatal("lost capability preserved success")
	}
	if f.Status.Application.Target != nil {
		t.Fatal("stale target retained as authoritative")
	}
}

func TestEnvtestApplicationStatusRoundTrip(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		t.Error("no pods exist")
		return controlplane.Application{}, nil
	})
	f := reconcile(t, r, x.fleet)
	if f.Status.Application == nil || f.Status.Application.ExpectedNodes != 3 {
		t.Fatalf("application status pruned: %+v", f.Status.Application)
	}
	// Exercise nested schema fields and list-map round trips against admission.
	f.Status.Application.Target = &fleet.ApplicationVersion{Version: "v2", Prefix: "deploy/v2/"}
	f.Status.Application.Versions = []fleet.ApplicationVersionCount{{ApplicationVersion: *f.Status.Application.Target, Nodes: 1}}
	f.Status.Application.Nodes = []fleet.ApplicationNodeStatus{{Name: "alpha-0", UID: "pod-0", RuntimeGeneration: "process-0", Version: "v2", Reason: "Converged"}}
	if err := r.Status().Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Application.Target.Version != "v2" || got.Status.Application.Versions[0].Prefix != "deploy/v2/" || got.Status.Application.Nodes[0].RuntimeGeneration != "process-0" {
		t.Fatal("nested application status pruned")
	}
}

func TestApplicationTargetExpiresDuringCollection(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	now := x.clock
	var reads atomic.Int32
	x.r.now = func() time.Time {
		if reads.Add(1) >= 5 {
			return now.Add(10 * time.Second)
		}
		return now
	}
	x.r.ApplicationRuntime = applicationReaderFunc(func(context.Context, controlplane.Target) (controlplane.Application, error) {
		a := applicationSnapshot(now)
		a.Target.ObservedAtMS = now.Add(-85 * time.Second).UnixMilli()
		return a, nil
	})
	_, status, _ := x.r.observeApplication(t.Context(), x.f)
	if status != metav1.ConditionUnknown {
		t.Fatal("target expired during collection but convergence stayed true")
	}
}
