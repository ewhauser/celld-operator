package controller

import (
	"context"
	"errors"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func paused(f *fleet.CelldFleet) bool { return f.Spec.Maintenance != nil && f.Spec.Maintenance.Paused }
func requestedMaintenance(f *fleet.CelldFleet, s *fleetState) (string, int32, string) {
	token := ""
	if f.Spec.Maintenance != nil {
		token = f.Spec.Maintenance.RestartToken
	}
	if !f.DeletionTimestamp.IsZero() {
		if s.Applied == 0 {
			return "", 0, ""
		}
		return "Delete", 0, token
	}
	if runtimeImage(f) != s.RuntimeImage {
		return "Upgrade", s.Applied, token
	}
	if token != "" && token != s.RestartToken {
		return "Restart", s.Applied, token
	}
	return "", 0, ""
}
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
	if f.Spec.Profile == "Bucket" {
		// Nothing is in flight outside the workload controller; pausing only
		// stops new workload changes.
		return r.report(ctx, f, nil, "MaintenancePaused", "Workload changes suspended", false)
	}
	return r.maintenanceFleet(ctx, f)
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
	return r.maintenanceFleet(ctx, f)
}
func (r *Reconciler) maintenanceFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(ctx, client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, nil, "MaintenanceBlocked", "No reservation authority; workload absence is not removal proof", false)
	}
	h := r.hydrate(ctx, res)
	want := fleetReservationSpec(f)
	if !r.reservationMatches(ctx, f, h, want) || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() {
		return r.report(ctx, f, h, "StorageScopeConflict", "Reservation ownership changed", false)
	}
	if h.j != nil && h.j.Operation == nil && h.j.Applied == 0 && h.j.Completion != nil && h.j.Completion.Kind == "Delete" && !f.DeletionTimestamp.IsZero() {
		return r.finishDeletion(ctx, f, h)
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "MaintenanceBlocked", "Workload missing without captured completion; retain storage and finalizer", false)
	}
	v, handled, err := r.lifecycle(ctx, f, h, w)
	if handled || err != nil {
		return v, err
	}
	return r.report(ctx, f, h, "MaintenancePaused", "New operations suspended; any issued operation must finish", false)
}
func (r *Reconciler) finishDeletion(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (ctrl.Result, error) {
	s := h.j
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(pods) != 0 {
		return r.report(ctx, f, h, "DeletionBlocked", "Unexpected pods after completed removal", false)
	}
	w := emptyObject(workload(f, r.Options))
	err = r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if err == nil {
		if w.GetUID() != s.WorkloadUID || replicas(w) != 0 || w.GetAnnotations()[operationKey] != s.Completion.ID+"/stop" {
			return ctrl.Result{}, errors.New("completed deletion workload identity changed")
		}
		return ctrl.Result{RequeueAfter: time.Second}, r.Delete(ctx, w, client.Preconditions{UID: new(w.GetUID()), ResourceVersion: new(w.GetResourceVersion())})
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// PVCs and CSI disks were removed under captured proof. The bucket
	// reservation remains permanent. Never DeleteAllOf or cascade storage.
	base := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	return ctrl.Result{}, r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
