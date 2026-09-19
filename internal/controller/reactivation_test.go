package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reactivationFixture drives a real 3→2 graceful contraction, then requests 3
// again so the retained ordinal-2 volume is reactivated through the production
// expand → Reactivating → finishReactivation path.
type reactivationFixture struct {
	*persistentFixture
	retired persistentMember
}

func reactivationSetup(t *testing.T) *reactivationFixture {
	t.Helper()
	p := persistentSetup(t)
	ctx := t.Context()
	// lifecycle() requires the creation journal and labeled retained claims that
	// initial provisioning would have produced.
	p.res.Annotations[attemptAnnotation] = "created"
	if err := p.r.Update(ctx, p.res); err != nil {
		t.Fatal(err)
	}
	for name := range p.j.Claims {
		claim := &corev1.PersistentVolumeClaim{}
		if err := p.r.Get(ctx, client.ObjectKey{Namespace: p.f.Namespace, Name: name}, claim); err != nil {
			t.Fatal(err)
		}
		claim.Labels = labels(p.f)
		claim.Annotations = map[string]string{"celld.example.com/storage-reservation": p.res.Name}
		if err := p.r.Update(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	// Contract through Intent, Stopping, Retiring and Recovering exactly as the
	// existing graceful test does.
	p.step(t)
	p.step(t)
	p.step(t)
	p.reader.barrier = true
	p.step(t)
	p.step(t)
	if replicas(p.w) != 2 {
		t.Fatal("no actual decrement")
	}
	old := &corev1.Pod{}
	if err := p.r.Get(ctx, client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, old); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	p.step(t)
	p.reader.now = p.reader.now.Add(11 * time.Second)
	p.step(t)
	if p.j.Operation != nil || p.j.Applied != 2 {
		t.Fatalf("contraction did not complete: %+v", p.j.Operation)
	}
	ix := slices.IndexFunc(p.j.PersistentHistory, func(m persistentMember) bool { return m.Node == "persistent-2" })
	if ix < 0 || !p.j.PersistentHistory[ix].Retired || !p.j.PersistentHistory[ix].Stopped {
		t.Fatal("positive retirement not retained")
	}
	return &reactivationFixture{persistentFixture: p, retired: p.j.PersistentHistory[ix]}
}

// reconcileLifecycle runs one production lifecycle pass with fresh journal and
// workload reads, mirroring a cold reconcile.
func (x *reactivationFixture) reconcileLifecycle(t *testing.T) error {
	t.Helper()
	ctx := t.Context()
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.res), x.res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(x.res)
	if err != nil {
		t.Fatal(err)
	}
	x.j = j
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.w), x.w); err != nil {
		t.Fatal(err)
	}
	_, _, err = x.r.lifecycle(ctx, x.f, x.res, x.w)
	return err
}

func (x *reactivationFixture) phase(t *testing.T) string {
	t.Helper()
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.res), x.res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(x.res)
	if err != nil {
		t.Fatal(err)
	}
	x.j = j
	if j.Operation == nil {
		return ""
	}
	return j.Operation.Phase
}

// startSuccessor stands in for the StatefulSet controller and kubelet: a fresh
// Pod UID on the recorded host, first container invocation, new launcher
// generation, and a live node record for that generation.
func (x *reactivationFixture) startSuccessor(t *testing.T, host, generation string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{Name: "persistent-2", Namespace: x.f.Namespace, UID: types.UID("replacement"), Labels: labels(x.f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: x.w.UID, Controller: new(true)}}, Spec: *x.w.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{PodIP: "10.0.0.9", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(x.reader.now)}}}}}}
	pod.Spec.NodeName = host
	pod.Spec.SchedulingGates = nil
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-persistent-2"}})
	if err := x.r.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	x.states["persistent-2"] = launcherStateFor(pod, "invocation-b", generation)
	x.reader.generation["persistent-2"] = generation
	x.reader.stopped = false // the successor renews its own lease again
	return pod
}

func TestPersistentReactivationCompletesWithNewGenerationOnSameHost(t *testing.T) {
	x := reactivationSetup(t)
	x.f = desiredCount(t, x.r, x.f, 3)
	if err := x.reconcileLifecycle(t); err != nil {
		t.Fatal(err)
	}
	if got := x.phase(t); got != "Intent" && got != "Prepared" {
		t.Fatalf("expected durable addition intent, got %q", got)
	}
	for range 3 {
		if x.phase(t) == "Reactivating" {
			break
		}
		if err := x.reconcileLifecycle(t); err != nil {
			t.Fatal(err)
		}
	}
	if x.phase(t) != "Reactivating" {
		t.Fatalf("replica issuance did not enter Reactivating: %+v", x.j.Operation)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.w), x.w); err != nil {
		t.Fatal(err)
	}
	if replicas(x.w) != 3 || x.w.Annotations[operationKey] != x.j.Operation.ID {
		t.Fatal("reactivation issued without the workload CAS carrying the operation")
	}
	// Until the successor is observed with a live record, the operation stays open.
	if err := x.reconcileLifecycle(t); err != nil {
		t.Fatal(err)
	}
	if x.phase(t) != "Reactivating" || x.j.Applied != 2 {
		t.Fatal("reactivation completed with no successor pod")
	}
	x.startSuccessor(t, x.retired.Host, "generation-2b")
	if err := x.reconcileLifecycle(t); err != nil {
		t.Fatal(err)
	}
	if x.phase(t) != "" || x.j.Applied != 3 {
		t.Fatalf("reactivation did not complete: %+v applied=%d", x.j.Operation, x.j.Applied)
	}
	last := x.j.History[len(x.j.History)-1]
	if last.Outcome != "RetainedVolumeReactivated" || last.From != 2 || last.To != 3 {
		t.Fatalf("completion record %+v", last)
	}
	var retiredKept, successor bool
	for _, m := range x.j.PersistentHistory {
		if m.Node != "persistent-2" {
			continue
		}
		switch m.Generation {
		case x.retired.Generation:
			retiredKept = m.Retired && m.Stopped
		case "generation-2b":
			successor = !m.Retired && m.PodUID == "replacement" && m.ClaimUID == x.retired.ClaimUID && m.VolumeUID == x.retired.VolumeUID && m.VolumeHandle == x.retired.VolumeHandle && m.Host == x.retired.Host
		}
	}
	if !retiredKept || !successor {
		t.Fatalf("history must retain the retired generation and record the successor on the same volume: %+v", x.j.PersistentHistory)
	}
}

func TestPersistentReactivationBlocksOnWrongIdentityEvidence(t *testing.T) {
	for name, fault := range map[string]func(*testing.T, *reactivationFixture){
		"successor reuses retired generation": func(t *testing.T, x *reactivationFixture) {
			x.startSuccessor(t, x.retired.Host, x.retired.Generation)
		},
		"successor on a different host": func(t *testing.T, x *reactivationFixture) {
			x.startSuccessor(t, "host-0", "generation-2b")
		},
		"successor without a live node record": func(t *testing.T, x *reactivationFixture) {
			x.startSuccessor(t, x.retired.Host, "generation-2b")
			x.reader.stopped = true // record still reads sealed and expired
		},
		"successor container restarted": func(t *testing.T, x *reactivationFixture) {
			pod := x.startSuccessor(t, x.retired.Host, "generation-2b")
			pod.Status.ContainerStatuses[0].RestartCount = 1
			if err := x.r.Status().Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := reactivationSetup(t)
			x.f = desiredCount(t, x.r, x.f, 3)
			for range 4 {
				if x.phase(t) == "Reactivating" {
					break
				}
				if err := x.reconcileLifecycle(t); err != nil {
					t.Fatal(err)
				}
			}
			if x.phase(t) != "Reactivating" {
				t.Fatalf("did not reach Reactivating: %+v", x.j.Operation)
			}
			fault(t, x)
			for range 2 {
				if err := x.reconcileLifecycle(t); err != nil {
					t.Fatal(err)
				}
			}
			if x.phase(t) != "Reactivating" || x.j.Applied != 2 {
				t.Fatalf("%s was accepted as reactivation evidence: %+v applied=%d", name, x.j.Operation, x.j.Applied)
			}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.w), x.w); err != nil {
				t.Fatal(err)
			}
			if replicas(x.w) != 3 {
				t.Fatal("blocked reactivation must not roll back the issued replica effect")
			}
			for _, m := range x.j.PersistentHistory {
				if m.Node == "persistent-2" && m.Generation != x.retired.Generation {
					t.Fatalf("unverified successor entered history: %+v", m)
				}
			}
			c := metav1.Condition{}
			got := &fleet.CelldFleet{}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), got); err != nil {
				t.Fatal(err)
			}
			for _, cond := range got.Status.Conditions {
				if cond.Type == "Ready" {
					c = cond
				}
			}
			if c.Reason != "ReactivationBlocked" && c.Reason != "LifecycleBlocked" && c.Reason != "PersistentSchedulingBlocked" {
				t.Fatalf("blocker not reported: %+v", c)
			}
		})
	}
}

func TestPersistentReactivationCrashBeforePhaseSaveIsReconstructed(t *testing.T) {
	x := reactivationSetup(t)
	x.f = desiredCount(t, x.r, x.f, 3)
	for range 3 {
		if x.phase(t) == "Prepared" {
			break
		}
		if err := x.reconcileLifecycle(t); err != nil {
			t.Fatal(err)
		}
	}
	if x.phase(t) != "Prepared" {
		t.Fatalf("expected Prepared before the replica CAS, got %+v", x.j.Operation)
	}
	opID := x.j.Operation.ID
	base := x.r.Client
	crashed := false
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok && !crashed {
			w := &appsv1.StatefulSet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(x.w), w); err == nil && replicas(w) == 3 {
				crashed = true
				return errors.New("leader crashed after replica CAS, before Reactivating was saved")
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	for range 3 {
		if crashed {
			break
		}
		_ = x.reconcileLifecycle(t)
	}
	if !crashed {
		t.Fatal("crash not injected")
	}
	x.r.Client = base
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.w), x.w); err != nil {
		t.Fatal(err)
	}
	if replicas(x.w) != 3 || x.w.Annotations[operationKey] != opID {
		t.Fatal("replica effect was not durable on the workload")
	}
	if x.phase(t) != "Prepared" {
		t.Fatal("journal advanced despite the crash")
	}
	// The successor comes up while the journal still says Prepared. A new leader
	// must recognize the issued effect from the workload and finish through
	// Reactivating without a second replica write.
	x.startSuccessor(t, x.retired.Host, "generation-2b")
	updates := 0
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == x.w.Name && obj.GetNamespace() == x.w.Namespace {
			if _, ok := obj.(*corev1.Pod); !ok {
				updates++
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	for range 3 {
		if x.phase(t) == "" {
			break
		}
		if err := x.reconcileLifecycle(t); err != nil {
			t.Fatal(err)
		}
	}
	if x.phase(t) != "" || x.j.Applied != 3 {
		t.Fatalf("reconstruction did not complete: %+v", x.j.Operation)
	}
	if updates != 0 {
		t.Fatalf("workload was written %d more times after the issued effect", updates)
	}
	completed := 0
	for _, h := range x.j.History {
		if h.ID == opID {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("operation recorded %d times", completed)
	}
}

func launcherStateFor(pod *corev1.Pod, invocation, generation string) launcher.State {
	return launcher.State{PodUID: string(pod.UID), Node: pod.Name, Host: pod.Spec.NodeName, Invocation: invocation, Generation: generation, Phase: "Running"}
}
