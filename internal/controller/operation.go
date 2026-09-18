package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const canceledOperationKey = "celld.example.com/canceled-operation"
const operationBudget = 30 * time.Minute

// cancelRemoval first persists cancellation intent, then fences the old issuer on
// its CAS object, then retires authority. Crashes at any boundary are replayable.
// No second replica effect is created until the old CAS can no longer succeed.
func (r *Reconciler) cancelRemoval(ctx context.Context, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	if w.GetAnnotations()[operationKey] == op.ID && replicas(w) == op.To {
		// Issuance won before cancellation. Preserve its recovery authority forever
		// if necessary; the next persistent ordinal would reactivate the victim.
		op.Phase = "Recovering"
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
	}
	if replicas(w) != op.From {
		return ctrl.Result{}, true, errors.New("cannot cancel changed workload")
	}
	if w.GetAnnotations()[canceledOperationKey] != op.ID {
		if w.GetAnnotations() == nil {
			w.SetAnnotations(map[string]string{})
		}
		w.GetAnnotations()[canceledOperationKey] = op.ID
		// The observed RV (not a retry with a fresh object) linearizes cancellation
		// against any delayed replica writer. The annotation is NOT a process fence.
		if err := r.Update(ctx, w); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	done := completion(op, time.Time{})
	done.Outcome = "CanceledBeforeIssue"
	j.History = append(j.History, done)
	j.Operation = nil
	resetMaintenanceCapacity(j)
	return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
}

func (r *Reconciler) productionRemovalBlock(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) (string, string) {
	if j.Loss != "" {
		return "PossibleDataLoss", j.Loss
	}
	if r.Evidence == nil {
		if f.Spec.Profile == "Bucket" {
			return "BucketCompletionUnqualified", "Mode-specific Bucket removal evidence remains unqualified"
		}
		return "FencingUnqualified", "Production evidence provider unavailable; no removal authorized"
	}
	policyFleet := f.DeepCopy()
	if policyFleet.Spec.Capacity == nil {
		policyFleet.Spec.Capacity = &fleet.CapacityPolicy{}
		policyFleet.Spec.Capacity.Default()
	}
	if r.Collector == nil {
		return "CapacityUncertain", "Survivor metrics collector unavailable"
	}
	observation := r.Collector.Collect(ctx, policyFleet)
	var identities []string
	donor := ""
	for _, s := range j.Inventory.Sessions {
		if !s.Current {
			continue
		}
		if !slices.Contains(identities, s.Container) {
			identities = append(identities, s.Container)
		}
		if s.Pod == j.Operation.TargetPod {
			donor = s.Container
		}
	}
	if len(identities) != int(j.Operation.From) {
		return "CapacityUncertain", "Current session coverage differs from applied replicas"
	}
	donors := []string{donor}
	// Deployment victim selection is not deterministic; shared preflight must
	// conservatively project every possible victim until its executor exists.
	if f.Spec.Profile == "Bucket" {
		donors = identities
	}
	for _, candidate := range donors {
		if err := ValidateSurvivors(*policyFleet.Spec.Capacity, observation, identities, candidate, r.capacityNow()); err != nil {
			return "CapacityUncertain", err.Error()
		}
	}
	if j.Inventory.CheckedAt.IsZero() || j.Inventory.CheckedAt.After(r.capacityNow()) || r.capacityNow().Sub(j.Inventory.CheckedAt) > 5*time.Second {
		return "RecoveryInventoryUnavailable", "Metadata expired during survivor revalidation"
	}
	if j.Inventory.Blocker != "" {
		return j.Inventory.Blocker, "Runtime session observations retained; exact authenticated binding and historical recovery remain required"
	}
	if f.Spec.Profile == "Bucket" {
		return "BucketCompletionUnqualified", "Bucket removal executor and mode-specific completion evidence remain unqualified"
	}
	_, err := r.Evidence.Stopped(ctx, f, j.Operation)
	return "FencingUnqualified", err.Error()
}
