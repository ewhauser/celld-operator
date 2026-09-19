package controller

import (
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// livenessPrep gives the persistent fixture the creation journal and labeled
// claims that lifecycle() demands, so tests can drive the production entry point.
func livenessPrep(t *testing.T, p *persistentFixture) {
	t.Helper()
	ctx := t.Context()
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
}

func lifecycleOnce(t *testing.T, p *persistentFixture) *lifecycleJournal {
	t.Helper()
	ctx := t.Context()
	if err := p.r.Get(ctx, client.ObjectKeyFromObject(p.res), p.res); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Get(ctx, client.ObjectKeyFromObject(p.w), p.w); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.r.lifecycle(ctx, p.f, p.res, p.w); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Get(ctx, client.ObjectKeyFromObject(p.res), p.res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(p.res)
	if err != nil {
		t.Fatal(err)
	}
	p.j = j
	return j
}

func readyReason(t *testing.T, r *Reconciler, f *fleet.CelldFleet) string {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			return c.Reason
		}
	}
	return ""
}

// A Stopping operation whose launcher never accepted a stop must not hold the
// fleet forever after its deadline. Every stop request expires no later than the
// deadline, so a Running launcher one grace period later proves nothing was issued.
func TestPersistentStoppingWithoutAcceptedStopCancelsAfterExpiry(t *testing.T) {
	p := persistentSetup(t)
	livenessPrep(t, p)
	// step installs fresh capacity samples, which admission needs before the stop.
	p.step(t)
	if p.j.Operation.Phase != "Intent" {
		t.Fatalf("expected Intent, got %+v", p.j.Operation)
	}
	p.step(t)
	if p.j.Operation.Phase != "Stopping" {
		t.Fatalf("expected Stopping, got %+v", p.j.Operation)
	}
	deadline := p.j.Operation.Deadline
	// The pre-stop assessment keeps failing, so no stop request is ever sent.
	p.reader.partial = true
	for range 2 {
		p.step(t)
		if p.j.Operation.Phase != "Stopping" {
			t.Fatalf("phase changed while blocked: %+v", p.j.Operation)
		}
	}
	if got := readyReason(t, p.r, p.f); got != "PersistentRecoveryBlocked" {
		t.Fatalf("expected assessment block, got %s", got)
	}
	// Past the deadline but inside the expiry margin: stalled, still not canceled.
	p.reader.now = deadline.Add(time.Second)
	for range 3 {
		if j := lifecycleOnce(t, p); j.Operation == nil || j.Operation.Phase != "Stopping" {
			t.Fatalf("canceled before every stop request had expired: %+v", j.Operation)
		}
	}
	if got := readyReason(t, p.r, p.f); got != "OperationStalled" {
		t.Fatalf("expected OperationStalled inside the expiry margin, got %s", got)
	}
	// Once the margin has elapsed the removal is provably unissued and cancels
	// through the ordinary workload-CAS fence.
	p.reader.now = deadline.Add(stopExpiryGrace)
	var j *lifecycleJournal
	for range 4 {
		j = lifecycleOnce(t, p)
		if j.Operation == nil {
			break
		}
	}
	if j.Operation != nil || j.Applied != 3 {
		t.Fatalf("stalled Stopping operation was not canceled: %+v applied=%d", j.Operation, j.Applied)
	}
	last := j.History[len(j.History)-1]
	if last.Outcome != "CanceledBeforeIssue" || last.From != 3 || last.To != 2 {
		t.Fatalf("unexpected completion record %+v", last)
	}
	if replicas(p.w) != 3 || p.w.Annotations[operationKey] != "" {
		t.Fatalf("workload changed by a canceled operation: replicas=%d annotations=%v", replicas(p.w), p.w.Annotations)
	}
	if p.states["persistent-2"].Phase != "Running" {
		t.Fatal("a stop was sent despite the failing assessment")
	}
	if p.w.Annotations[canceledOperationKey] != last.ID {
		t.Fatal("cancellation did not fence the workload against a delayed issuer")
	}
	// The next desire is evaluated afresh rather than resuming the dead operation.
	p.reader.partial = false
	if j := lifecycleOnce(t, p); j.Operation == nil || j.Operation.ID == last.ID {
		t.Fatalf("new intent not recorded after cancellation: %+v", j.Operation)
	}
}

// A survivor whose pod is recreated (eviction, kubelet restart) must be able to
// return on its recorded host incarnation and retained volume without an
// operation, and that return must not block later lifecycle work.
func TestPersistentSurvivorReplacementReturnsOnSameHost(t *testing.T) {
	x := reactivationSetup(t)
	ctx := t.Context()
	survivor := x.j.PersistentHistory[slices.IndexFunc(x.j.PersistentHistory, func(m persistentMember) bool { return m.Node == "persistent-0" && !m.Retired })]
	old := &corev1.Pod{}
	if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "persistent-0"}, old); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	gated := &corev1.Pod{Name: "persistent-0", Namespace: x.f.Namespace, UID: types.UID("persistent-0-b"), Labels: labels(x.f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: x.w.UID, Controller: new(true)}}, Spec: *x.w.Spec.Template.Spec.DeepCopy()}
	gated.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	gated.Spec.NodeName = ""
	gated.Spec.Volumes = append(gated.Spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-persistent-0"}})
	if err := x.r.Create(ctx, gated); err != nil {
		t.Fatal(err)
	}
	if err := x.reconcileLifecycle(t); err != nil {
		t.Fatal(err)
	}
	if got := readyReason(t, x.r, x.f); got == "PersistentSchedulingBlocked" {
		t.Fatal("survivor replacement froze the lifecycle")
	}
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(gated), gated); err != nil {
		t.Fatal(err)
	}
	if len(gated.Spec.SchedulingGates) != 0 || gated.Spec.NodeSelector[corev1.LabelHostname] != survivor.Hostname {
		t.Fatalf("survivor not admitted back onto its recorded host: gates=%v selector=%v", gated.Spec.SchedulingGates, gated.Spec.NodeSelector)
	}
	x.phase(t)
	marked := x.j.PersistentHistory[slices.IndexFunc(x.j.PersistentHistory, func(m persistentMember) bool { return m.Node == "persistent-0" && m.Generation == survivor.Generation })]
	if !marked.Superseded || marked.Retired {
		t.Fatalf("superseded survivor not recorded durably: %+v", marked)
	}
	// The kubelet places the pod and the successor launcher starts a new generation.
	gated.Spec.NodeName = survivor.Host
	if err := x.r.Update(ctx, gated); err != nil {
		t.Fatal(err)
	}
	gated.Status = corev1.PodStatus{PodIP: "10.0.0.8", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-0b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(x.reader.now)}}}}}
	if err := x.r.Status().Update(ctx, gated); err != nil {
		t.Fatal(err)
	}
	x.states["persistent-0"] = launcherStateFor(gated, "invocation-0b", "generation-0b")
	x.reader.generation["persistent-0"] = "generation-0b"
	// Retained-volume reactivation of the retired ordinal still completes: the
	// superseded survivor generation is resolved history, not an unknown writer.
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
		t.Fatalf("reactivation did not issue after survivor replacement: %+v (reason %s)", x.j.Operation, readyReason(t, x.r, x.f))
	}
	x.startSuccessor(t, x.retired.Host, "generation-2b")
	if err := x.reconcileLifecycle(t); err != nil {
		t.Fatal(err)
	}
	if x.phase(t) != "" || x.j.Applied != 3 {
		t.Fatalf("reactivation blocked by the replaced survivor: %+v reason=%s", x.j.Operation, readyReason(t, x.r, x.f))
	}
}

func TestPersistentSurvivorReplacementBlocksOnChangedHostIncarnation(t *testing.T) {
	for name, fault := range map[string]func(*testing.T, *reactivationFixture, persistentMember){
		"node rebooted": func(t *testing.T, x *reactivationFixture, s persistentMember) {
			n := &corev1.Node{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Name: s.Host}, n); err != nil {
				t.Fatal(err)
			}
			n.Status.NodeInfo.BootID = "rebooted"
			if err := x.r.Status().Update(t.Context(), n); err != nil {
				t.Fatal(err)
			}
		},
		"claim replaced": func(t *testing.T, x *reactivationFixture, s persistentMember) {
			c := &corev1.PersistentVolumeClaim{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "data-" + s.Node}, c); err != nil {
				t.Fatal(err)
			}
			c.Spec.VolumeName = "pv-other"
			if err := x.r.Update(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := reactivationSetup(t)
			ctx := t.Context()
			survivor := x.j.PersistentHistory[slices.IndexFunc(x.j.PersistentHistory, func(m persistentMember) bool { return m.Node == "persistent-0" && !m.Retired })]
			old := &corev1.Pod{}
			if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "persistent-0"}, old); err != nil {
				t.Fatal(err)
			}
			if err := x.r.Delete(ctx, old); err != nil {
				t.Fatal(err)
			}
			gated := &corev1.Pod{Name: "persistent-0", Namespace: x.f.Namespace, UID: types.UID("persistent-0-b"), Labels: labels(x.f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: x.w.UID, Controller: new(true)}}, Spec: *x.w.Spec.Template.Spec.DeepCopy()}
			gated.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
			gated.Spec.NodeName = ""
			if err := x.r.Create(ctx, gated); err != nil {
				t.Fatal(err)
			}
			fault(t, x, survivor)
			if err := x.reconcileLifecycle(t); err != nil {
				t.Fatal(err)
			}
			if got := readyReason(t, x.r, x.f); got != "PersistentSchedulingBlocked" {
				t.Fatalf("%s admitted: %s", name, got)
			}
			if err := x.r.Get(ctx, client.ObjectKeyFromObject(gated), gated); err != nil {
				t.Fatal(err)
			}
			if len(gated.Spec.SchedulingGates) != 1 {
				t.Fatal("gate released on an uncertain host or volume")
			}
			x.phase(t)
			for _, m := range x.j.PersistentHistory {
				if m.Node == "persistent-0" && m.Superseded {
					t.Fatal("survivor marked superseded without admission")
				}
			}
		})
	}
}
