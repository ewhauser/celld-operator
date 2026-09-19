package controller

import (
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func editMaintenance(t *testing.T, r *Reconciler, f *fleet.CelldFleet, edit func(*fleet.CelldFleet)) *fleet.CelldFleet {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	edit(got)
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMaintenancePauseFencesDelayedIssuerAndResumes(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = desiredCount(t, r, f, 5)
			reconcile(t, r, f)
			j := getJournal(t, r, f)
			op := *j.Operation
			stale := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), stale); err != nil {
				t.Fatal(err)
			}
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
			reason(t, reconcile(t, r, f), "MaintenancePaused")
			if err := r.applyReplicas(t.Context(), stale, &op); err == nil {
				t.Fatal("delayed issuer bypassed pause fence")
			}
			for range 3 {
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true}
				reason(t, reconcile(t, r, f), "MaintenancePaused")
			}
			if got := getJournal(t, r, f); got.Applied != 3 || got.Operation.ID != op.ID {
				t.Fatal("pause changed operation")
			}
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance.Paused = false; f.Spec.Replicas = 4 })
			for range 8 {
				reconcile(t, r, f)
			}
			if got := getJournal(t, r, f); got.Applied != 5 {
				t.Fatalf("recorded additive target lost: %+v", got)
			}
		})
	}
}

func TestMaintenanceUnsupportedRequestsRetainPinAndJournal(t *testing.T) {
	for _, kind := range []string{"upgrade", "architecture-digest", "restart"} {
		t.Run(kind, func(t *testing.T) {
			r, f := lifecycleSetup(t, "PersistentFleet")
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) {
				if kind == "restart" {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "restart-1"}
				} else {
					f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:" + strings.Repeat("a", 64)
					if kind == "architecture-digest" {
						f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:0c915aed95925945d145f242811b95cd4a6659ddd577dfbb6104c00c447b1e43"
					}
				}
				f.Spec.Replicas = 5
			})
			expected := "UnsupportedTransition"
			if kind == "restart" {
				expected = "DisruptionUnqualified"
			}
			reason(t, reconcile(t, r, f), expected)
			j := getJournal(t, r, f)
			id := j.Request.ID
			for range 3 {
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true}
				reason(t, reconcile(t, r, f), expected)
			}
			j = getJournal(t, r, f)
			if j.Request.ID != id || j.Operation != nil || j.Applied != 3 || j.Request.SourceImage != Image {
				t.Fatal("blocked request lost identity or started capacity")
			}
			actual := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), actual); err != nil {
				t.Fatal(err)
			}
			expectedFleet := f.DeepCopy()
			expectedFleet.Spec.Replicas = 3
			if !matches(workload(expectedFleet, r.Options), actual) {
				t.Fatal("request changed pinned workload")
			}
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = ""; f.Spec.Maintenance = nil })
			for range 6 {
				reconcile(t, r, f)
			}
			if getJournal(t, r, f).Applied != 5 {
				t.Fatal("withdrawn blocked request prevented safe addition")
			}
		})
	}
}

func TestMaintenanceDeletionRetainsEvidenceAndRejectsAdoption(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	before := getJournal(t, r, f)
	if err := r.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, r, f), "DeletionBlocked")
	j := getJournal(t, r, f)
	if j.Request == nil || j.Request.Kind != "Delete" || len(j.Claims) != len(before.Claims) {
		t.Fatal("deletion lost durable evidence")
	}
	for range 3 {
		reason(t, reconcile(t, r, f), "DeletionBlocked")
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if replicas(w) != 3 || w.GetAnnotations()[maintenanceFenceKey] != "deleting" || len(w.GetOwnerReferences()) != 0 {
		t.Fatal("unsafe deletion")
	}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	impostor := f.DeepCopy()
	impostor.UID = "replacement"
	want := res.Spec
	want.FleetUID = "replacement"
	if r.reservationMatches(t.Context(), impostor, res, want) {
		t.Fatal("retained reservation adopted")
	}
}

func TestMaintenancePauseResetsCachedCapacity(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j := getJournal(t, r, f)
	j.Capacity = &capacity.State{Actionable: true, LowSince: time.Now(), HighSince: time.Now(), LowSamples: 5, HighSamples: 5}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
	reconcile(t, r, f)
	s := getJournal(t, r, f).Capacity
	if s.Actionable || s.LowSamples != 0 || s.HighSamples != 0 {
		t.Fatal("pause retained actionable cached evidence")
	}
}

func TestMaintenanceRejectsGarbageCollectedClaim(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	claims := initialClaims(f, workload(f, r.Options))
	claim := claims[0]
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "old", UID: "old"}}
	if err := r.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	f = desiredCount(t, r, f, 5)
	reason(t, reconcile(t, r, f), "StorageIdentityConflict")
}

func TestMaintenanceRecoveryContinuesAcrossPauseAndDeletion(t *testing.T) {
	for _, control := range []string{"pause", "delete"} {
		for _, fault := range []string{"none", "missing", "loss"} {
			t.Run(control+"/"+fault, func(t *testing.T) {
				f := fixture("alpha", "bucket-alpha", "PersistentFleet")
				f.Spec.Placement.AZCount = 1
				f.Spec.Placement.Zones = []string{"us-east-1a"}
				r := setup(t, f)
				r.Options.LocalTest = true
				e := &localEvidence{now: time.Now(), stopped: true}
				r.localLifecycle = e
				reconcile(t, r, f)
				reconcile(t, r, f)
				f = desiredCount(t, r, f, 2)
				reconcile(t, r, f)
				// Simulate a crash after the effect, before Recovering was persisted.
				j := getJournal(t, r, f)
				id := j.Operation.ID
				w := emptyObject(workload(f, r.Options))
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
					t.Fatal(err)
				}
				if err := r.applyReplicas(t.Context(), w, j.Operation); err != nil {
					t.Fatal(err)
				}
				if control == "pause" {
					f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
				} else {
					if err := r.Delete(t.Context(), f); err != nil {
						t.Fatal(err)
					}
				}
				e.incomplete = fault == "missing"
				e.loss = fault == "loss"
				for range 5 {
					r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, localLifecycle: e}
					reconcile(t, r, f)
					e.now = e.now.Add(11 * time.Second)
				}
				j = getJournal(t, r, f)
				if fault == "none" {
					if j.Applied != 2 || j.Operation != nil || len(j.Claims) != 3 || len(j.History) != 1 {
						t.Fatalf("issued recovery stranded: %+v", j)
					}
				} else {
					if j.Applied != 3 || j.Operation == nil || j.Operation.ID != id || !j.Operation.SettledAt.IsZero() {
						t.Fatal("maintenance bypassed uncertain recovery")
					}
					if fault == "loss" && j.Loss == "" {
						t.Fatal("loss forgotten")
					}
				}
			})
		}
	}
}

func TestMaintenanceIssuedAdditionCompletesWithoutAnotherEffect(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = desiredCount(t, r, f, 5)
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if err := r.applyReplicas(t.Context(), w, j.Operation); err != nil {
		t.Fatal(err)
	}
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
	reconcile(t, r, f)
	reconcile(t, r, f)
	j = getJournal(t, r, f)
	if j.Applied != 5 || j.Operation != nil || len(j.History) != 1 {
		t.Fatal("issued addition not recovered")
	}
}

func TestMaintenanceResumeAfterFenceOnlyCrashResetsCapacity(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j := getJournal(t, r, f)
	j.Capacity = &capacity.State{Actionable: true, LowSince: time.Now(), HighSince: time.Now(), LowSamples: 5, HighSamples: 5}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if err := r.setMaintenanceFence(t.Context(), f, w, "paused"); err != nil {
		t.Fatal(err)
	}
	// Crash here, then user resumes before another pause reconcile.
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance.Paused = false })
	reconcile(t, r, f)
	s := getJournal(t, r, f).Capacity
	if s.Actionable || s.LowSamples != 0 || s.HighSamples != 0 {
		t.Fatal("resume reused pre-pause positive history after crash")
	}
}

func TestMaintenanceConfigEditsSerializeBehindCapacity(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	f = desiredCount(t, r, f, 5)
	reconcile(t, r, f)
	id := getJournal(t, r, f).Operation.ID
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) {
		f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:" + strings.Repeat("b", 64)
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "queued"}
		f.Spec.Replicas = 4
	})
	for range 6 {
		reconcile(t, r, f)
	}
	j := getJournal(t, r, f)
	if j.Applied != 5 || j.Operation != nil || len(j.History) != 1 || j.History[0].ID != id || j.Request == nil || j.Request.Kind != "Upgrade" {
		t.Fatalf("configuration bypassed serialized operation: %+v", j)
	}
}

func TestMaintenanceConcurrentReconcilesKeepOneDeletionRequest(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	f = desiredCount(t, r, f, 5)
	reconcile(t, r, f)
	original := getJournal(t, r, f).Operation.ID
	if err := r.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			if err != nil && !apierrors.IsConflict(err) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	reason(t, reconcile(t, r, f), "DeletionBlocked")
	j := getJournal(t, r, f)
	requestID := j.Request.ID
	for range 3 {
		reconcile(t, r, f)
	}
	j = getJournal(t, r, f)
	if j.Operation.ID != original || j.Applied != 3 || j.Request.ID != requestID || len(j.Claims) != 3 {
		t.Fatal("concurrent deletion duplicated or lost intent")
	}
}

func TestMaintenanceUnknownAppliedRuntimeBlocksRollbackAndEvidenceInterpretation(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j := getJournal(t, r, f)
	j.RuntimeImage = "ghcr.io/denoland/celld@sha256:" + strings.Repeat("a", 64)
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(res); err == nil {
		t.Fatal("unknown runtime selected v0.5.0 evidence interpretation")
	}
	reason(t, reconcile(t, r, f), "StorageScopeConflict")
}

func TestMaintenanceLegacyJournalUpgradePreservesIdentity(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j := getJournal(t, r, f)
	j.Version = 1
	j.RuntimeImage = ""
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
	reconcile(t, r, f)
	got := getJournal(t, r, f)
	if got.Version != 7 || got.RuntimeImage != Image || got.Applied != j.Applied || len(got.Claims) != len(j.Claims) {
		t.Fatal("legacy identity lost")
	}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Annotations[journalKey], `"Version":7`) {
		t.Fatal("old binaries can still accept maintenance journal")
	}
}

func TestMaintenanceInitialPauseAndUnsupportedPinNeverLaunch(t *testing.T) {
	for _, control := range []string{"pause", "image"} {
		t.Run(control, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "Bucket")
			if control == "pause" {
				f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
			} else {
				f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:" + strings.Repeat("c", 64)
			}
			r := setup(t, f)
			reconcile(t, r, f)
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); !apierrors.IsNotFound(err) {
				t.Fatalf("unexpected workload: %v", err)
			}
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = nil; f.Spec.RuntimeImage = Image })
			for range 3 {
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Request != nil || j.Applied != 3 || j.RuntimeImage != Image {
				t.Fatal("explicit original pin was treated as an upgrade")
			}
		})
	}
}
