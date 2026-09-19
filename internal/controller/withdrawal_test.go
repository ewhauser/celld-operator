package controller

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Restoring the original replica count withdraws an unissued removal instead of
// freezing it, so later requests (here: a shadow capacity policy) are evaluated.
func TestRestoringReplicasWithdrawsBlockedRemoval(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = desiredCount(t, r, f, 1)
			reconcile(t, r, f)
			j := getJournal(t, r, f)
			if j.Operation == nil || j.Operation.Phase != "Blocked" {
				t.Fatalf("expected a blocked removal, got %+v", j.Operation)
			}
			blocked := j.Operation.ID
			f = desiredCount(t, r, f, 3)
			for range 4 {
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true}
				reconcile(t, r, f)
			}
			j = getJournal(t, r, f)
			if j.Operation != nil || j.Applied != 3 {
				t.Fatalf("reversal did not withdraw the unissued removal: %+v", j.Operation)
			}
			if last := j.History[len(j.History)-1]; last.ID != blocked || last.Outcome != "CanceledBeforeIssue" {
				t.Fatalf("unexpected completion record %+v", last)
			}
			w := workload(f, r.Options)
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if replicas(w) != 3 || w.GetAnnotations()[canceledOperationKey] != blocked {
				t.Fatalf("withdrawal must fence the workload without changing replicas: %d %v", replicas(w), w.GetAnnotations())
			}
			f = enableCapacity(t, r, f, "")
			got := reconcile(t, r, f)
			if got.Status.Capacity.Mode != "Shadow" {
				t.Fatalf("shadow policy not projected after withdrawal: %+v", got.Status.Capacity)
			}
		})
	}
}
