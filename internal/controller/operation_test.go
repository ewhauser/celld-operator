package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBlockedRemovalAdditionCancellationRestartBoundaries(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			recorder := events.NewFakeRecorder(64)
			r.Recorder = recorder
			f = desiredCount(t, r, f, 2)
			reconcile(t, r, f)
			op := *getJournal(t, r, f).Operation
			stale := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), stale); err != nil {
				t.Fatal(err)
			}
			// Model an issuer authorized by a former executor at the same RV.
			op.Phase = "Intent"
			op.WorkloadVersion = stale.GetResourceVersion()
			f = desiredCount(t, r, f, 4)
			reconcile(t, r, f) // durable cancellation intent
			if getJournal(t, r, f).Operation.Phase != "Canceling" {
				t.Fatal("no cancellation authority")
			}
			reconcile(t, r, f) // workload CAS fence, no replica effect
			if err := r.applyReplicas(t.Context(), stale, &op); err == nil {
				t.Fatal("delayed issuer won cancellation fence")
			}
			for range 7 {
				// Every remaining boundary loses the old process and status authority.
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, Recorder: recorder}
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			// The later addition is the only completion the journal keeps; the
			// cancellation of the old authority is audited as an event instead.
			if j.Applied != 4 || j.Operation != nil || len(j.History) != 1 || j.History[0].From != 3 || j.History[0].To != 4 {
				t.Fatalf("%+v", j)
			}
			if !recordedEvent(recorder, "CanceledBeforeIssue", op.ID) {
				t.Fatal("old authority lost")
			}
		})
	}
}
func TestIssuanceWinningCancellationRetainsRecovery(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	op := *j.Operation
	op.Phase = "Intent"
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	op.WorkloadVersion = w.GetResourceVersion()
	f = desiredCount(t, r, f, 4)
	reconcile(t, r, f) // cancellation journal, workload fence not yet committed
	if err := r.applyReplicas(t.Context(), w, &op); err != nil {
		t.Fatal(err)
	} // delayed issuance won
	for range 4 {
		reconcile(t, r, f)
	}
	j = getJournal(t, r, f)
	if j.Operation == nil || j.Operation.ID != op.ID || j.Operation.Phase != "Recovering" || j.Applied != 3 {
		t.Fatalf("issued authority lost: %+v", j)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if replicas(w) != 2 {
		t.Fatal("competing addition reactivated removed disk")
	}
}
func TestOperationDeadlineAndCanceledContext(t *testing.T) {
	for _, phase := range []string{"Intent", "Prepared", "issued"} {
		t.Run(phase, func(t *testing.T) {
			r, f := lifecycleSetup(t, "Bucket")
			now := time.Now()
			r.now = func() time.Time { return now }
			f = desiredCount(t, r, f, 4)
			reconcile(t, r, f)
			if phase != "Intent" {
				reconcile(t, r, f)
			}
			op := *getJournal(t, r, f).Operation
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if phase == "issued" {
				if err := r.applyReplicas(t.Context(), w, &op); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(operationBudget + time.Second)
			r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, now: func() time.Time { return now }}
			for range 3 {
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if phase == "issued" {
				if j.Applied != 4 || j.Operation != nil {
					t.Fatal("issued effect not recovered after deadline")
				}
			} else {
				if j.Operation == nil || !j.Operation.Stalled || j.Applied != 3 {
					t.Fatal("expired operation executed")
				}
				if err := r.applyReplicas(t.Context(), w, &op); err == nil {
					t.Fatal("late issuer ignored deadline")
				}
			}
		})
	}
	r, f := lifecycleSetup(t, "Bucket")
	f = desiredCount(t, r, f, 4)
	reconcile(t, r, f)
	op := getJournal(t, r, f).Operation
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.applyReplicas(ctx, w, op); err == nil {
		t.Fatal("canceled context issued effect")
	}
}
func TestJournalV1ThroughV6(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	j := getJournal(t, r, f)
	for _, version := range []int{1, 2, 3, 4, 5, 6} {
		res := &fleet.CelldStorageReservation{}
		if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
			t.Fatal(err)
		}
		j.Version = version
		if err := r.saveJournal(t.Context(), res, j); err != nil {
			t.Fatal(err)
		}
		restored, err := readJournal(res)
		if err != nil || restored.Version != 8 {
			t.Fatalf("legacy %d: %v", version, err)
		}
	}
}

func TestJournalBudgetNeverPrunesHistory(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	before := res.Annotations[journalKey]
	j := getJournal(t, r, f)
	j.Inventory.Blocker = strings.Repeat("x", 201*1024)
	if err := r.saveJournal(t.Context(), res, j); err == nil {
		t.Fatal("oversized journal accepted")
	}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if res.Annotations[journalKey] != before {
		t.Fatal("failed save changed durable authority")
	}
}

func TestJournalRejectsInvalidDeadlineAndRequest(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, want string
		mutate     func(*lifecycleJournal)
	}{
		{"equal-deadline", "invalid operation deadline", func(j *lifecycleJournal) { j.Operation.Deadline = now }},
		{"backwards-deadline", "invalid operation deadline", func(j *lifecycleJournal) { j.Operation.Deadline = now.Add(-time.Second) }},
		{"missing-request-id", "invalid disruption request", func(j *lifecycleJournal) { j.Request.ID = "" }},
		{"wrong-source", "invalid disruption request", func(j *lifecycleJournal) { j.Request.SourceImage = "other" }},
		{"wrong-workload", "invalid disruption request", func(j *lifecycleJournal) { j.Request.WorkloadUID = "other" }},
		{"unknown-kind", "invalid disruption request", func(j *lifecycleJournal) { j.Request.Kind = "Unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &lifecycleJournal{Version: 8, RuntimeImage: Image, Initial: 3, Applied: 3, WorkloadUID: "workload",
				Operation: &lifecycleOperation{ID: "op", From: 3, To: 2, Phase: "Intent", StartedAt: now, Deadline: now.Add(time.Minute)},
				Request:   &disruptionRequest{ID: "request", Kind: "Restart", SourceImage: Image, WorkloadUID: "workload"}}
			read := func() error {
				b, err := json.Marshal(j)
				if err != nil {
					t.Fatal(err)
				}
				_, err = readJournal(&fleet.CelldStorageReservation{Annotations: map[string]string{journalKey: string(b)}})
				return err
			}
			if err := read(); err != nil {
				t.Fatalf("valid control rejected: %v", err)
			}
			tc.mutate(j)
			if err := read(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
