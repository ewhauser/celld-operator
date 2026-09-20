package controller

import (
	"context"
	"errors"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	"k8s.io/apimachinery/pkg/api/equality"
)

// admitBucketHistory records only currently qualified exact invocations. It does
// not request a disruption or adopt unknown previous writers. Early admission
// lets a later same-Pod-UID container restart supersede a known Bucket writer.
func (r *Reconciler) admitBucketHistory(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) (bool, error) {
	if j.Operation != nil || j.Maintenance != nil || j.Loss != "" || j.Applied < 1 || r.Evidence == nil {
		return false, nil
	}
	if j.Inventory.CheckedAt.IsZero() || j.Inventory.CheckedAt.After(r.capacityNow()) || r.capacityNow().Sub(j.Inventory.CheckedAt) > 5*time.Second {
		return false, errors.New("steady Bucket inventory is not fresh")
	}
	view := *j
	view.Operation = &lifecycleOperation{ID: "observational-admission", Phase: "Recovering", From: j.Applied, To: j.Applied}
	sessions, _, err := r.bucketAssessmentMode(ctx, f, &view, j.Applied, true, bucketScopeAdmission)
	if err != nil {
		changed := false
		if invalidated, ok := errors.AsType[*v050.BucketExpiryInvalidatedError](err); ok {
			changed = invalidateBucketExpiry([][]bucketSession{j.BucketHistory}, invalidated)
		}
		return changed, err
	}
	if equality.Semantic.DeepEqual(sessions, j.BucketHistory) {
		return false, nil
	}
	j.BucketHistory = sessions
	return true, nil
}
