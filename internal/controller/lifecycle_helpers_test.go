package controller

import (
	"testing"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func getJournal(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *fleetState {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j, err := readState(res)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// recordedEvents drains everything a fake recorder has collected so far. The
// audit trail that History no longer keeps is emitted as events instead, so
// tests assert on this rather than on a growing journal field.
func desiredCount(t *testing.T, r *Reconciler, f *fleet.CelldFleet, n int32) *fleet.CelldFleet {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = n
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	return got
}
func lifecycleSetup(t *testing.T, profile string) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", profile)
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	reconcile(t, r, f)
	reconcile(t, r, f)
	return r, f
}

// TestCompletionHistoryKeepsOnlyTheLastEntry pins the ADR 0021 phase 1 boundary
// for History: the journal retains exactly the entry the status projection
// reads, and the trail that used to grow beside it is emitted as events.
func TestBootstrapConsumesCreationClaimInventory(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet") // create, then bootstrap
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if _, present := res.Annotations[creationClaimsKey]; present {
		t.Fatalf("bootstrap retained the consumed claim inventory: %v", res.Annotations)
	}
	j, err := readState(res)
	if err != nil || j == nil || len(j.Claims) != 3 {
		t.Fatalf("bootstrap did not carry the claim identities into the journal: %v %+v", err, j)
	}
	reason(t, reconcile(t, r, f), "Provisioning") // waiting on readiness, not blocked
	if after := getJournal(t, r, f); after == nil || len(after.Claims) != 3 {
		t.Fatalf("journal no longer loads without the annotation: %+v", after)
	}

	// A crash between PVC creation and bootstrap leaves a workload with neither
	// a journal nor an inventory to verify it against; that still blocks.
	other := fixture("beta", "bucket-beta", "PersistentFleet")
	other.Spec.Placement.AZCount = 1
	other.Spec.Placement.Zones = []string{"us-east-1a"}
	x := setup(t, other)
	reconcile(t, x, other)
	crashed := &fleet.CelldStorageReservation{}
	if err := x.Get(t.Context(), types.NamespacedName{Name: reservationName(other)}, crashed); err != nil {
		t.Fatal(err)
	}
	delete(crashed.Annotations, creationClaimsKey)
	if err := x.Update(t.Context(), crashed); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x, other), "StorageIdentityConflict")
	if j := getJournal(t, x, other); j != nil {
		t.Fatalf("unverified workload was adopted: %+v", j)
	}
}

var _ = time.Second

const fixtureRuntime = "ghcr.io/ewhauser/celld@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const fixtureLauncher = "ghcr.io/ewhauser/celld-operator@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func completeLauncherRemoval(s *launcher.State, operation string) {
	s.Phase, s.Operation = "Stopped", operation
	s.ChildExited, s.InheritedLockReleased, s.RestartDenied = true, true, true
	s.Removal = launcher.RemovalResult{Operation: operation, Generation: s.Generation, Mode: "remove-disk", Phase: "data_safe", ControlOnly: true, DataSafe: true}
}
