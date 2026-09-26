package controller

import (
	"context"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func paused(f *fleet.CelldFleet) bool { return f.Spec.Maintenance != nil && f.Spec.Maintenance.Paused }
func (r *Reconciler) pauseFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	// Nothing is in flight outside the workload controller; pausing only stops
	// new workload changes.
	return r.report(ctx, f, nil, "MaintenancePaused", "Workload changes suspended", false)
}
func (r *Reconciler) deleteFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	if f.Spec.Storage.Initialization != nil {
		res := &fleet.CelldStorageReservation{}
		err := r.Get(ctx, client.ObjectKey{Name: reservationName(f)}, res)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if !seedReceiptReady(f, res) {
			handled, err := r.deleteUninitializedFleet(ctx, f)
			if err != nil {
				return r.report(ctx, f, nil, "SeedCancellationBlocked", err.Error(), false)
			}
			if handled {
				return ctrl.Result{}, nil
			}
		}
	}
	return r.deleteWorkload(ctx, f)
}
