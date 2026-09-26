package controller

import (
	"context"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func paused(f *fleet.CelldFleet) bool { return f.Spec.Maintenance != nil && f.Spec.Maintenance.Paused }
func resetMaintenanceCapacity(s *fleetState) {
	if s.Capacity == nil {
		return
	}
	c := s.Capacity
	// Current samples cannot span a disruption. Preserve the bounded addition
	// baseline and ineffective-batch hold, but collect fresh samples afterward.
	c.Load = nil
	c.Stamps = nil
	c.Actionable = false
	c.LowSince = time.Time{}
	c.HighSince = time.Time{}
	c.LowSamples = 0
	c.HighSamples = 0
	if c.Addition != nil {
		c.Addition.Since = time.Time{}
		c.Addition.Samples = 0
	}
}
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
	if f.Spec.Profile == "Bucket" {
		return r.deleteBucket(ctx, f)
	}
	return r.deletePersistent(ctx, f)
}
