package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const maintenanceFenceKey = "celld.example.com/maintenance-fence"

// Requests never authorize a workload mutation. A qualified executor must use
// Operation, with exact target/session capture, before this can change.
type disruptionRequest struct {
	ID, Kind, SourceImage, TargetImage, RestartToken string
	WorkloadUID                                      types.UID
	Generation                                       int64
}

func paused(f *fleet.CelldFleet) bool { return f.Spec.Maintenance != nil && f.Spec.Maintenance.Paused }
func maintenanceFence(f *fleet.CelldFleet) string {
	if !f.DeletionTimestamp.IsZero() {
		return "deleting"
	}
	if paused(f) {
		return "paused"
	}
	return ""
}

func (r *Reconciler) setMaintenanceFence(ctx context.Context, f *fleet.CelldFleet, w client.Object, value string) error {
	latest := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), latest); err != nil {
		return err
	}
	if latest.UID != f.UID || maintenanceFence(latest) != value {
		return errors.New("maintenance request changed; reconcile again")
	}
	if w.GetAnnotations() == nil {
		w.SetAnnotations(map[string]string{})
	}
	if value == "" {
		delete(w.GetAnnotations(), maintenanceFenceKey)
	} else {
		w.GetAnnotations()[maintenanceFenceKey] = value
	}
	// Same object CAS as replica issuance: an already issued effect is recovered,
	// while any delayed issuer holding the old version loses.
	return r.Update(ctx, w)
}

// persistDisruptionRequest changes only the retained request. Callers report the
// final reconcile outcome once, after any in-flight recovery has been assessed.
func (r *Reconciler) persistDisruptionRequest(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (string, bool, error) {
	kind, target, token := "", runtimeImage(f), ""
	if f.Spec.RuntimeImage != "" {
		target = f.Spec.RuntimeImage
	}
	if target != j.RuntimeImage {
		kind = "Upgrade"
	}
	if f.Spec.Maintenance != nil {
		token = f.Spec.Maintenance.RestartToken
	}
	if kind == "" && token != "" && !slices.Contains(j.CompletedRestarts, token) {
		kind = "Restart"
	}
	if !f.DeletionTimestamp.IsZero() {
		kind = "Delete"
	}
	if kind == "" {
		if j.Request != nil {
			j.Request = nil
			return kind, true, r.saveJournal(ctx, res, j)
		}
		return kind, false, nil
	}
	if j.Request == nil || j.Request.Kind != kind || j.Request.TargetImage != target || j.Request.RestartToken != token {
		j.Request = &disruptionRequest{ID: string(uuid.NewUUID()), Kind: kind, SourceImage: j.RuntimeImage, TargetImage: target, RestartToken: token, WorkloadUID: w.GetUID(), Generation: f.Generation}
		return kind, true, r.saveJournal(ctx, res, j)
	}
	return kind, false, nil
}

func (r *Reconciler) disruption(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	kind, changed, err := r.persistDisruptionRequest(ctx, f, res, j, w)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if kind == "" {
		if changed {
			// Reservations are not watched. A new leader must not depend on an
			// earlier timer or another fleet edit to resume queued capacity.
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		return ctrl.Result{}, false, nil
	}

	if kind == "Upgrade" && canStopUpgrade(f, j, r.Options) && r.Evidence != nil {
		return r.beginStoppedUpgrade(ctx, f, res, j, w)
	}

	if (f.Spec.Profile == "Bucket" || (f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "")) && (kind == "Restart" || kind == "Delete") && r.Evidence != nil {
		return r.beginMaintenance(ctx, f, res, j, w)
	}
	reason := "DisruptionUnqualified"
	if kind == "Upgrade" {
		reason = "UnsupportedTransition"
	}
	if kind == "Delete" {
		reason = "DeletionBlocked"
	}
	result, err := r.report(ctx, f, reason, "Request retained in shared lifecycle journal; no upgrade, rollback, restart or final shutdown is qualified. Workloads, PVCs, storage reservation and recovery evidence remain retained", 0, false)
	return result, true, err
}

func (r *Reconciler) pauseFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	return r.maintenanceFleet(ctx, f)
}
func (r *Reconciler) deleteFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	return r.maintenanceFleet(ctx, f)
}

func (r *Reconciler) maintenanceFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	reason := "MaintenancePaused"
	if !f.DeletionTimestamp.IsZero() {
		reason = "DeletionBlocked"
	}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(ctx, types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if !f.DeletionTimestamp.IsZero() {
			candidate := emptyObject(workload(f, r.Options))
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), candidate); err == nil {
				return r.report(ctx, f, "DeletionBlocked", "Missing reservation for existing workload; refusing reconstruction of runtime authority", 0, false)
			} else if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			pods := &corev1.PodList{}
			if err := r.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
				return ctrl.Result{}, err
			}
			if len(pods.Items) != 0 {
				return r.report(ctx, f, "DeletionBlocked", "Missing reservation with live pod identities requires investigation", 0, false)
			}
			res = &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{InitialReplicas: f.Spec.Replicas, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
			res.Annotations = map[string]string{attemptAnnotation: "deletion-before-workload"}
			if err := r.Create(ctx, res); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.report(ctx, f, reason, "No reservation found; provisioning suspended", 0, false)
	}
	if result, handled, err := r.migrateBucket(ctx, f, res); handled || err != nil {
		return result, err
	}
	want := fleet.ReservationSpec{InitialReplicas: f.Spec.Replicas, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}
	if len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() || !r.reservationMatches(ctx, f, res, want) {
		return r.report(ctx, f, "StorageScopeConflict", "Cannot bind maintenance to retained storage authority", 0, false)
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if !f.DeletionTimestamp.IsZero() {
			j, err := r.loadJournal(ctx, res)
			if err != nil {
				return ctrl.Result{}, err
			}
			if j != nil && j.Maintenance != nil && j.Maintenance.Kind == "Delete" && j.Maintenance.Phase == "Cleanup" {
				result, _, err := r.completeRetainedDeletion(ctx, f, res, j)
				return result, err
			}
			if res.Annotations[attemptAnnotation] == "" || res.Annotations[attemptAnnotation] == "deletion-before-workload" {
				if res.Annotations == nil {
					res.Annotations = map[string]string{}
				}
				res.Annotations[attemptAnnotation] = "deletion-before-workload"
				j = &lifecycleJournal{Version: 8, RuntimeImage: Image, Initial: res.Spec.InitialReplicas, Applied: res.Spec.InitialReplicas, Maintenance: &maintenanceOperation{ID: string(uuid.NewUUID()), Kind: "Delete", Phase: "Cleanup"}}
				if err := r.saveJournal(ctx, res, j); err != nil {
					return ctrl.Result{}, err
				}
				result, _, err := r.completeRetainedDeletion(ctx, f, res, j)
				return result, err
			}
		}
		return r.report(ctx, f, reason, "Workload absent; retain finalizer and reservation because absence is not process fencing or recovery evidence", 0, false)
	}
	if w.GetLabels()[FleetLabel] != string(f.UID) || len(w.GetOwnerReferences()) != 0 {
		return r.report(ctx, f, "LifecycleBlocked", "Workload identity or garbage collection ownership changed", 0, false)
	}
	if w.GetAnnotations()[maintenanceFenceKey] != maintenanceFence(f) {
		if err := r.setMaintenanceFence(ctx, f, w, maintenanceFence(f)); err != nil {
			return ctrl.Result{}, err
		}
	}
	result, handled, err := r.lifecycle(ctx, f, res, w)
	if handled || err != nil {
		return result, err
	}
	return r.report(ctx, f, reason, "Workload fence acknowledged; new actions suspended, issued operations continue recovery; all data and identities retained", 0, false)
}

func resetMaintenanceCapacity(j *lifecycleJournal) {
	if j.Capacity == nil {
		return
	}
	if j.Capacity.Addition != nil {
		j.Capacity.Addition.Since = time.Time{}
		j.Capacity.Addition.Samples = 0
	}
	j.Capacity.Actionable = false
	j.Capacity.LowSince, j.Capacity.HighSince = time.Time{}, time.Time{}
	j.Capacity.LowSamples, j.Capacity.HighSamples = 0, 0
}
