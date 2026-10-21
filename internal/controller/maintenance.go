package controller

import (
	"context"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func paused(f *fleet.CelldFleet) bool { return f.Spec.Maintenance != nil && f.Spec.Maintenance.Paused }
func (r *Reconciler) pauseFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	// The early replacement cleanup releases our Pod holds without starting a
	// disk deletion. Pausing here stops new workload changes; an already accepted
	// Kubernetes deletion cannot be undone.
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
			if apierrors.IsForbidden(err) {
				return ctrl.Result{}, err
			}
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

// releaseInTerminatingNamespace releases a deleted fleet's finalizer when its
// namespace is terminating and the operator has lost access to it. The
// namespace controller deletes the fleet-namespace Role binding along with the
// workload, Pods and claims, so the fleet could otherwise never finish
// deletion. A seed is still stopped first: its reservation is cluster-scoped
// and outlives the namespace.
func (r *Reconciler) releaseInTerminatingNamespace(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, bool, error) {
	if f.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(f, Finalizer) {
		return ctrl.Result{}, false, nil
	}
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: f.Namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, false, err
	}
	if ns.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, false, nil
	}
	if f.Spec.Storage.Initialization != nil {
		res := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKey{Name: reservationName(f)}, res); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, err
		}
		if !seedReceiptReady(f, res) {
			if _, err := r.stopFleetSeed(ctx, f); err != nil {
				result, err := r.report(ctx, f, nil, "SeedCancellationBlocked", err.Error(), false)
				return result, true, err
			}
		}
	}
	base := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	return ctrl.Result{}, true, r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
