package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PersistentFleet (ADR 0023): one voluntary disruption at a time, gated on
// celld's node-log reports; no existing disk deleted while a session needs
// it; a disk that is gone never blocks the fleet.

func persistentFleet(t *testing.T) *operationFixture {
	t.Helper()
	x := newOperationFixture(t, "PersistentFleet")
	x.converge()
	return x
}

func (x *operationFixture) claimUIDs() map[string]types.UID {
	x.t.Helper()
	list := &corev1.PersistentVolumeClaimList{}
	if err := x.r.List(x.t.Context(), list, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := map[string]types.UID{}
	for _, c := range list.Items {
		if c.DeletionTimestamp.IsZero() {
			out[c.Name] = c.UID
		}
	}
	return out
}

func (x *operationFixture) podUIDs() map[string]types.UID {
	x.t.Helper()
	list := &corev1.PodList{}
	if err := x.r.List(x.t.Context(), list, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := map[string]types.UID{}
	for _, p := range list.Items {
		out[p.Name] = p.UID
	}
	return out
}

func (x *operationFixture) budget() int {
	x.t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), pdb); err != nil {
		x.t.Fatal(err)
	}
	return pdb.Spec.MaxUnavailable.IntValue()
}

func readyReason(f *fleet.CelldFleet) string {
	if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c != nil {
		return c.Reason
	}
	return ""
}

func TestPersistentProvisionsWithoutLauncher(t *testing.T) {
	x := persistentFleet(t)
	sts := x.workload().(*appsv1.StatefulSet)
	pod := sts.Spec.Template.Spec
	if len(pod.InitContainers) != 0 || pod.Containers[0].Command != nil || len(pod.SchedulingGates) != 0 || pod.Containers[0].LivenessProbe != nil {
		t.Fatal("persistent pod still runs under the launcher")
	}
	if mode, _ := envValue(pod.Containers[0].Env, "CELLD_DURABILITY"); mode != "fleet" {
		t.Fatal("persistent fleet must use fleet durability")
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatal("operator must control which member restarts")
	}
	if x.budget() != 1 {
		t.Fatal("settled fleet allows one voluntary disruption")
	}
	if x.reads == 0 {
		t.Fatal("settlement was not read from celld")
	}
}

func TestPersistentBudgetClosesWhileRecovering(t *testing.T) {
	x := persistentFleet(t)
	x.unrecovered = []controlplane.UnrecoveredLog{{Session: "alpha-1/g", State: "recovering"}}
	if reason := readyReason(x.step()); reason != "LifecycleProgress" {
		t.Fatalf("recovering fleet reported %s", reason)
	}
	if x.budget() != 0 {
		t.Fatal("node drains must wait while the fleet recovers")
	}
	x.unrecovered = nil
	x.step()
	if x.budget() != 1 {
		t.Fatal("budget not reopened after settling")
	}
}

func TestPersistentRollingRestartKeepsDisks(t *testing.T) {
	for _, change := range []string{"restart", "upgrade"} {
		t.Run(change, func(t *testing.T) {
			x := persistentFleet(t)
			claims := x.claimUIDs()
			pods := x.podUIDs()
			x.edit(func(f *fleet.CelldFleet) {
				if change == "restart" {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
				} else {
					f.Spec.RuntimeImage = fixtureRuntimeUpgrade
				}
			})
			x.step()
			x.syncWorkload()
			replaced := []string{}
			for range 3 {
				// While the fleet recovers, no running member restarts.
				x.unrecovered = []controlplane.UnrecoveredLog{{Session: "x/g", State: "open"}}
				before := x.podUIDs()
				x.step()
				if after := x.podUIDs(); len(after) != len(before) {
					t.Fatal("restarted a member while the fleet was recovering")
				}
				x.unrecovered = nil
				x.step()
				after := x.podUIDs()
				gone := 0
				for name, uid := range before {
					if after[name] != uid {
						gone++
						replaced = append(replaced, name)
					}
				}
				if gone != 1 {
					t.Fatalf("expected exactly one restart per settled step, got %d", gone)
				}
				x.syncWorkload()
			}
			if len(replaced) != 3 || replaced[0] != "alpha-2" || replaced[2] != "alpha-0" {
				t.Fatalf("members restarted out of order: %v", replaced)
			}
			for name, uid := range x.podUIDs() {
				if uid == pods[name] {
					t.Fatalf("%s was not restarted", name)
				}
			}
			for name, uid := range x.claimUIDs() {
				if claims[name] != uid {
					t.Fatalf("restart replaced disk %s", name)
				}
			}
			if f := x.converge(); f == nil || x.state().RestartToken == "" && change == "restart" {
				t.Fatal("restart token not recorded")
			}
			if change == "upgrade" && x.state().RuntimeImage != fixtureRuntimeUpgrade {
				t.Fatal("upgrade image not recorded")
			}
		})
	}
}

func TestPersistentDownMemberIsUpdatedAtOnce(t *testing.T) {
	x := persistentFleet(t)
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"} })
	x.step()
	x.syncWorkload()
	p := x.pod("alpha-0")
	p.Status.Conditions = nil
	if err := x.r.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	x.unrecovered = []controlplane.UnrecoveredLog{{Session: "alpha-0/g", State: "open"}}
	x.step()
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(p), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("an already-down outdated member waits for settlement it cannot reach")
	}
}

func TestPersistentLegacyRuntimeRollsButNeverReleasesDisks(t *testing.T) {
	x := persistentFleet(t)
	x.noNodeLog = true
	x.edit(func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = fixtureRuntimeUpgrade })
	x.step()
	x.syncWorkload()
	// A long-stable fleet may restart its first member at once.
	before := x.podUIDs()
	x.step()
	if len(x.podUIDs()) == len(before) {
		t.Fatal("legacy runtime cannot roll onto a node-log build")
	}
	x.syncWorkload()
	before = x.podUIDs()
	x.step()
	if len(x.podUIDs()) != len(before) {
		t.Fatal("legacy runtime restarted the next member before stabilization")
	}
	x.clock = x.clock.Add(2 * legacyStabilization)
	x.step()
	if len(x.podUIDs()) == len(before) {
		t.Fatal("legacy runtime did not continue after stabilization")
	}
	for range 10 {
		x.syncWorkload()
		x.clock = x.clock.Add(2 * legacyStabilization)
		x.step()
	}
	x.desired(2)
	for range 5 {
		x.clock = x.clock.Add(2 * legacyStabilization)
		x.step()
		x.syncWorkload()
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("a runtime without node-log state authorized contraction")
	}
}

func TestPersistentScaleInReleasesDiskOnlyWhenUnneeded(t *testing.T) {
	x := persistentFleet(t)
	x.desired(2)
	x.obligations = map[string][]string{"alpha-2": {"alpha-0/g"}}
	x.step()
	if replicas(x.workload()) != 2 || x.state().LastDisruption.IsZero() {
		t.Fatal("settled fleet did not remove its highest member")
	}
	x.syncWorkload()
	for range 3 {
		x.step()
	}
	if _, kept := x.claimUIDs()["data-alpha-2"]; !kept {
		t.Fatal("deleted a disk a session still needs")
	}
	f := x.step()
	if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c == nil || c.Reason != "LifecycleProgress" {
		t.Fatalf("retention not reported: %+v", c)
	}
	// Growth never reuses a removed member's disk.
	x.desired(3)
	x.step()
	if replicas(x.workload()) != 2 {
		t.Fatal("growth reused a retained disk")
	}
	x.stale = true
	x.obligations = nil
	x.step()
	if _, kept := x.claimUIDs()["data-alpha-2"]; !kept {
		t.Fatal("released a disk without a fresh complete sweep")
	}
	x.stale = false
	x.step()
	if _, kept := x.claimUIDs()["data-alpha-2"]; kept {
		t.Fatal("unneeded disk of a removed member was retained")
	}
	x.syncWorkload()
	x.converge()
	if replicas(x.workload()) != 3 {
		t.Fatal("growth did not resume after release")
	}
}

func TestPersistentLostDiskIsReplacedWithoutWaiting(t *testing.T) {
	for _, how := range []string{"claim lost", "volume gone"} {
		t.Run(how, func(t *testing.T) {
			x := persistentFleet(t)
			claim := &corev1.PersistentVolumeClaim{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "data-alpha-1"}, claim); err != nil {
				t.Fatal(err)
			}
			switch how {
			case "claim lost":
				claim.Status.Phase = corev1.ClaimLost
				if err := x.r.Status().Update(t.Context(), claim); err != nil {
					t.Fatal(err)
				}
			case "volume gone":
				pv := &corev1.PersistentVolume{}
				if err := x.r.Get(t.Context(), client.ObjectKey{Name: claim.Spec.VolumeName}, pv); err != nil {
					t.Fatal(err)
				}
				pv.Finalizers = nil
				if err := x.r.Update(t.Context(), pv); err != nil {
					t.Fatal(err)
				}
				if err := x.r.Delete(t.Context(), pv); err != nil {
					t.Fatal(err)
				}
			}
			// Recovery is pending and the lost disk held the only copy of a
			// session: the operator still does not wait.
			x.unrecovered = []controlplane.UnrecoveredLog{{Session: "alpha-1/g", State: "open"}}
			x.obligations = map[string][]string{"alpha-1": {"alpha-0/g"}}
			f := x.step()
			if _, kept := x.claimUIDs()["data-alpha-1"]; kept {
				t.Fatal("lost disk's claim retained")
			}
			if _, kept := x.podUIDs()["alpha-1"]; kept {
				t.Fatal("member on a lost disk not replaced")
			}
			if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c == nil || !strings.Contains(c.Message, "alpha-0/g") {
				t.Fatalf("possible loss not reported: %+v", c)
			}
			x.unrecovered, x.obligations = nil, nil
			x.syncWorkload()
			x.converge()
			if _, back := x.claimUIDs()["data-alpha-1"]; !back {
				t.Fatal("replacement member has no fresh disk")
			}
		})
	}
}

func TestPersistentReplaceMember(t *testing.T) {
	x := persistentFleet(t)
	x.edit(func(f *fleet.CelldFleet) { f.Annotations = map[string]string{replaceMemberAnnotation: "9"} })
	if reason := readyReason(x.step()); reason != "ReplaceMemberInvalid" {
		t.Fatalf("invalid ordinal reported as %s", reason)
	}
	claims := x.claimUIDs()
	x.edit(func(f *fleet.CelldFleet) { f.Annotations = map[string]string{replaceMemberAnnotation: "alpha-1"} })
	x.unrecovered = []controlplane.UnrecoveredLog{{Session: "alpha-2/g", State: "open"}}
	x.step()
	if x.claimUIDs()["data-alpha-1"] != claims["data-alpha-1"] {
		t.Fatal("replacement ignored the one-disruption rule")
	}
	x.unrecovered = nil
	x.obligations = map[string][]string{"alpha-1": {"alpha-0/g"}}
	x.step()
	if _, kept := x.claimUIDs()["data-alpha-1"]; kept {
		t.Fatal("administrator replacement refused despite obligations")
	}
	got := &fleet.CelldFleet{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[replaceMemberAnnotation] != "" {
		t.Fatal("completed replacement request not cleared")
	}
	x.f = got
	x.obligations = nil
	x.syncWorkload()
	x.converge()
}

func TestPersistentMissingWorkloadIsRecreatedOnItsDisks(t *testing.T) {
	x := persistentFleet(t)
	claims := x.claimUIDs()
	if err := x.r.Delete(t.Context(), x.workload()); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(x.step()); reason != "Provisioning" {
		t.Fatalf("missing workload reported %s", reason)
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("workload not recreated")
	}
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("recreation replaced disk %s", name)
		}
	}
}

func TestPersistentDeletionRemovesComputeThenDisks(t *testing.T) {
	x := persistentFleet(t)
	if err := x.r.Delete(t.Context(), x.f); err != nil {
		t.Fatal(err)
	}
	x.step()
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("workload not deleted first")
	}
	if len(x.claimUIDs()) != 3 {
		t.Fatal("disks deleted before compute")
	}
	x.step()
	if len(x.claimUIDs()) != 0 {
		t.Fatal("disks not deleted after compute")
	}
	if _, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)}); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
		t.Fatal("finalizer not released")
	}
	if err := x.r.Get(t.Context(), types.NamespacedName{Name: reservationName(x.f)}, &fleet.CelldStorageReservation{}); err != nil {
		t.Fatal("bucket reservation must stay permanent", err)
	}
}

func TestPersistentMigratesStrictFleet(t *testing.T) {
	x := persistentFleet(t)
	// An 0022 member: gated, launcher-supervised, from an older template.
	p := x.pod("alpha-2")
	if err := x.r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	p.ResourceVersion, p.UID = "", "old-alpha-2"
	p.Labels[revisionLabel] = "old"
	p.Spec.NodeName = ""
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	if err := x.r.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	res := envReservation(t, x.r, x.f)
	s := x.state()
	s.Operation = &legacyOperation{ID: "old-op", Kind: "Restart"}
	if err := x.r.saveState(t.Context(), res, s); err != nil {
		t.Fatal(err)
	}
	x.step()
	if s := x.state(); s.Operation != nil || s.Completion == nil || s.Completion.Outcome != "Superseded" {
		t.Fatalf("strict operation not superseded: %+v", s)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(p), p); err == nil && len(p.Spec.SchedulingGates) != 0 {
		t.Fatal("launcher gate left on a migrated member")
	}
	x.syncWorkload()
	x.converge()
	for _, pod := range []string{"alpha-0", "alpha-1", "alpha-2"} {
		if x.pod(pod).Labels[revisionLabel] != x.workload().(*appsv1.StatefulSet).Status.UpdateRevision {
			t.Fatalf("%s not rolled onto the current template", pod)
		}
	}
}

// Horizon: a sweep made before the last disruption plus one lease TTL cannot
// settle the fleet, so a crash whose lease has not expired is never missed.
func TestPersistentSettlementWaitsForFreshSweep(t *testing.T) {
	x := persistentFleet(t)
	x.desired(2)
	x.step()
	x.syncWorkload()
	x.desired(1)
	x.r.RuntimeState = staleAfter{x, x.clock}
	for range 3 {
		x.step()
	}
	if replicas(x.workload()) != 2 {
		t.Fatal("second removal before a sweep observed the first")
	}
}

type staleAfter struct {
	x    *operationFixture
	when time.Time
}

func (s staleAfter) NodeLog(ctx context.Context, target controlplane.Target) (controlplane.NodeLog, error) {
	log, err := fakeNodeLogs{s.x}.NodeLog(ctx, target)
	if log.Fleet != nil {
		log.Fleet.ObservedAt = s.when
	}
	return log, err
}
