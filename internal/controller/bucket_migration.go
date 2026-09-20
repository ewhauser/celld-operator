package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const migrationKey = "celld.eric.dev/bucket-migration"

type bucketMigration struct {
	ID, Token, Phase     string
	CreationAuthorized   bool
	SourceUID, TargetUID types.UID
	SettledAt            time.Time
}

func validateBucketMigration(j *lifecycleJournal) error {
	m := j.BucketMigration
	if m == nil {
		return nil
	}
	if m.ID == "" || m.Token == "" || m.SourceUID == "" || len(j.Claims) != 0 {
		return errors.New("invalid Bucket migration identity")
	}
	if m.Phase == "Activating" && (!m.CreationAuthorized || m.TargetUID == "") {
		return errors.New("missing exact migration activation identity")
	}
	switch m.Phase {
	case "Capture", "Authorized", "Recovering", "DeleteOld", "CreateNew", "Activating", "Retained":
		if j.Operation != nil || j.Maintenance != nil || j.WorkloadUID != m.SourceUID {
			return errors.New("overlapping Bucket migration authority")
		}
	case "Complete":
		if m.TargetUID == "" || j.WorkloadUID != m.TargetUID {
			return errors.New("invalid completed Bucket migration")
		}
	default:
		return errors.New("invalid Bucket migration phase")
	}
	return nil
}

// Migration keeps the original immutable reservation hash and every admitted
// writer. Only workload kind/UID changes; no bucket, fleet, or disk is adopted.
func (r *Reconciler) migrateBucket(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation) (ctrl.Result, bool, error) {
	j, err := r.loadJournal(ctx, res)
	if err != nil {
		result, reportErr := r.report(ctx, f, "StorageScopeConflict", "Cannot load retained lifecycle authority: "+err.Error(), false)
		return result, true, reportErr
	}
	if j == nil {
		return ctrl.Result{}, false, nil
	}
	m := j.BucketMigration
	if m == nil && (f.Spec.Profile != "Bucket" || f.Spec.BucketWorkload != "Ordered" || f.Spec.Maintenance == nil || f.Spec.Maintenance.OrderedMigrationToken == "") {
		return ctrl.Result{}, false, nil
	}
	if m != nil && m.Phase == "Complete" {
		return ctrl.Result{}, false, nil
	}
	block := func(e error) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, "MigrationBlocked", e.Error(), false)
		return result, true, err
	}
	old := f.DeepCopy()
	old.Spec.BucketWorkload = "Deployment"
	old.Spec.Replicas = j.Applied
	old.Spec.RuntimeImage = j.RuntimeImage
	want := fleet.ReservationSpec{Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID)}
	if len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() || !r.reservationMatches(ctx, old, res, want) || j.RuntimeImage != Image || ((m == nil || m.Phase == "Capture") && f.Spec.RuntimeImage != "" && f.Spec.RuntimeImage != j.RuntimeImage) || f.Spec.Profile != "Bucket" || f.Spec.BucketWorkload != "Ordered" || j.Loss != "" {
		return block(errors.New("migration requires the unchanged v0.5.0 Bucket reservation and loss-free history"))
	}
	save := func() (ctrl.Result, bool, error) {
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	if m == nil && !f.DeletionTimestamp.IsZero() {
		// No migration action was admitted. Retire the still-authoritative source
		// layout, including any existing lifecycle recovery, rather than inventing
		// a target simply because the requested layout changed before deletion.
		result, err := r.maintenanceFleet(ctx, old)
		return result, true, err
	}
	if m == nil {
		if paused(f) || !f.DeletionTimestamp.IsZero() || !f.Spec.Maintenance.AllowCoordinatedDowntime || j.Operation != nil || j.Maintenance != nil || j.Request != nil || len(j.Claims) != 0 || (f.Spec.RuntimeImage != "" && f.Spec.RuntimeImage != Image) {
			return block(errors.New("migration requires explicit coordinated downtime and no concurrent lifecycle request"))
		}
		target := &appsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), target); !apierrors.IsNotFound(err) {
			if err != nil {
				return ctrl.Result{}, true, err
			}
			return block(errors.New("migration target already exists; refusing adoption"))
		}
		j.BucketMigration = &bucketMigration{ID: string(uuid.NewUUID()), Token: f.Spec.Maintenance.OrderedMigrationToken, Phase: "Capture", SourceUID: j.WorkloadUID}
		return save()
	}
	if m.Phase == "Retained" {

		if _, err := r.migrationRetirementEvidence(ctx, old, j); err != nil {
			return r.migrationFailure(ctx, f, res, j, nil, err)
		}
		target := &appsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), target); err == nil {
			expected := f.DeepCopy()
			expected.Spec.RuntimeImage = j.RuntimeImage
			expected.Spec.Replicas = 0
			if target.Annotations[migrationKey] != m.ID || !matches(workload(expected, r.Options), target) {
				return block(errors.New("non-inert target exists before retained deletion"))
			}
			uid, rv := target.UID, target.ResourceVersion
			if err := r.Delete(ctx, target, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		} else if !apierrors.IsNotFound(err) {
			return block(err)
		}
		source := &appsv1.Deployment{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), source); !apierrors.IsNotFound(err) {
			return block(errors.New("source exists or cannot be checked before retained deletion"))
		}
		copyJournal := *j
		copyJournal.Maintenance = &maintenanceOperation{ID: m.ID, Kind: "Delete", Phase: "Cleanup"}
		return r.completeRetainedDeletion(ctx, f, res, &copyJournal)
	}
	if m.Phase == "Activating" {
		return r.activateMigratedBucket(ctx, f, res, j)
	}
	if m.Phase == "CreateNew" {
		return r.createMigratedBucket(ctx, f, res, j)
	}
	w := &appsv1.Deployment{}
	err = r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if apierrors.IsNotFound(err) && m.Phase == "DeleteOld" {
		m.Phase = "CreateNew"
		return save()
	}
	if err != nil {
		return block(fmt.Errorf("source workload unavailable: %w", err))
	}
	expected := workload(old, r.Options)
	if m.Phase == "Recovering" || m.Phase == "DeleteOld" || (m.Phase == "Authorized" && w.Annotations[operationKey] == m.ID) {
		setReplicas(expected, 0)
	}
	if w.UID != m.SourceUID || !matches(expected, w) || w.Annotations[lossFenceKey] != "" {
		return block(errors.New("migration source identity, template, or loss fence changed"))
	}
	if r.Evidence == nil {
		return block(errors.New("migration requires production Bucket evidence"))
	}
	switch m.Phase {
	case "Capture":
		latest := &fleet.CelldFleet{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), latest); err != nil {
			return ctrl.Result{}, true, err
		}
		if latest.UID != f.UID || latest.Generation != f.Generation || latest.Spec.BucketWorkload != "Ordered" || (latest.DeletionTimestamp.IsZero() && (paused(latest) || latest.Spec.Maintenance == nil || !latest.Spec.Maintenance.AllowCoordinatedDowntime || latest.Spec.Maintenance.OrderedMigrationToken != m.Token)) {
			return block(errors.New("migration request changed before admission"))
		}
		inventory, loss := r.Evidence.Observe(ctx, old, j.Inventory)
		j.Inventory = inventory
		if loss != "" {
			return r.recordLoss(ctx, f, w, res, j, loss)
		}
		sessions, _, err := r.migrationBucketAssessment(ctx, old, j)
		if err != nil {
			return r.migrationFailure(ctx, f, res, j, w, err)
		}
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		// Workload CAS invalidates all delayed replica issuers before journal admission.
		w.Annotations[maintenanceFenceKey] = "migrating"
		if err := r.Update(ctx, w); err != nil {
			return ctrl.Result{}, true, err
		}
		j.BucketHistory = sessions
		m.Phase = "Authorized"
		return save()
	case "Authorized":
		if w.Annotations[maintenanceFenceKey] != "migrating" {
			return block(errors.New("migration workload fence missing"))
		}
		if replicas(w) != 0 {
			// Re-capture immediately before removing all membership. Unknown writers
			// created in a concurrent ReplicaSet loop remain blocked by recovery inventory.
			sessions, _, err := r.migrationBucketAssessment(ctx, old, j)
			if err != nil {
				return r.migrationFailure(ctx, f, res, j, w, err)
			}
			if len(sessions) != len(j.BucketHistory) || !slices.EqualFunc(sessions, j.BucketHistory, func(a, b bucketSession) bool {
				return slices.Contains(j.BucketHistory, a) && slices.Contains(sessions, b)
			}) {
				return block(errors.New("membership changed after migration capture"))
			}
			setReplicas(w, 0)
			w.Annotations[operationKey] = m.ID
			if err := r.Update(ctx, w); err != nil {
				return ctrl.Result{}, true, err
			}
		} else if w.Annotations[operationKey] != m.ID {
			return block(errors.New("zero replicas lacks exact migration authority"))
		}
		m.Phase = "Recovering"
		return save()
	case "Recovering":
		evidence, err := r.migrationRetirementEvidence(ctx, old, j)
		if err != nil {
			return r.migrationFailure(ctx, f, res, j, w, err)
		}
		for i := range j.BucketHistory {
			j.BucketHistory[i].Retired = true
			j.BucketHistory[i].ExpiryObserved = true
			j.BucketHistory[i].ExpiryInvalidated = false
		}
		if t := awaitSettling(&m.SettledAt, evidence.ObservedAt, save, requeueSoon); t != nil {
			return t.unwrap()
		}
		m.Phase = "DeleteOld"
		return save()
	case "DeleteOld":
		if _, err := r.migrationRetirementEvidence(ctx, old, j); err != nil {
			return r.migrationFailure(ctx, f, res, j, w, err)
		}
		uid, rv := w.UID, w.ResourceVersion
		if err := r.Delete(ctx, w, client.Preconditions{UID: &uid, ResourceVersion: &rv}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	return block(errors.New("unknown migration phase"))
}

func (r *Reconciler) createMigratedBucket(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal) (ctrl.Result, bool, error) {
	m := j.BucketMigration
	old := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), old); !apierrors.IsNotFound(err) {
		return ctrl.Result{}, true, errors.New("source Deployment still exists or cannot be checked")
	}
	current := f.DeepCopy()
	current.Spec.Replicas = 0
	current.Spec.RuntimeImage = j.RuntimeImage
	desired := workload(current, r.Options)
	desired.SetAnnotations(map[string]string{migrationKey: m.ID})
	target := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(f), target)
	if apierrors.IsNotFound(err) {
		if _, err := r.migrationRetirementEvidence(ctx, f, j); err != nil {
			return r.migrationFailure(ctx, f, res, j, nil, err)
		}
		if !m.CreationAuthorized && !f.DeletionTimestamp.IsZero() {
			m.Phase = "Retained"
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		if !m.CreationAuthorized {
			m.CreationAuthorized = true
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, true, err
		}
		target = desired.(*appsv1.StatefulSet)
	} else if err != nil {
		return ctrl.Result{}, true, err
	}
	// A replay after successful Create is bound to its unpredictable, durably
	// reserved operation ID and exact full template, never just name/labels.
	if !m.CreationAuthorized || target.UID == "" || target.Annotations[migrationKey] != m.ID || !matches(desired, target) {
		return ctrl.Result{}, true, errors.New("migration target identity or template mismatch")
	}
	m.TargetUID = target.UID
	m.Phase = "Activating"
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, true, nil
}

func (r *Reconciler) migrationBucketAssessment(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) ([]bucketSession, time.Time, error) {
	view := *j
	view.Operation = &lifecycleOperation{ID: j.BucketMigration.ID, Phase: "Recovering", From: j.Applied, To: j.Applied}
	return r.bucketAssessmentMode(ctx, f, &view, j.Applied, true, bucketScopeAdmission)
}

// Re-read loss declarations and every admitted retired generation at each compute
// boundary. A saved phase is not a timeless storage certificate.
func (r *Reconciler) migrationRetirementEvidence(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) (v050.BucketObservation, error) {
	if r.Evidence == nil {
		return v050.BucketObservation{}, errors.New("migration evidence unavailable")
	}
	pods, err := r.Evidence.pods(ctx, f)
	if err != nil {
		return v050.BucketObservation{}, err
	}
	if len(pods) != 0 {
		return v050.BucketObservation{}, errors.New("waiting for all source Pods to disappear")
	}
	inventory, loss := r.Evidence.Observe(ctx, f, j.Inventory)
	j.Inventory = inventory
	if loss != "" {
		return v050.BucketObservation{}, &v050.LossError{Key: loss}
	}
	for _, session := range inventory.Sessions {
		if !slices.ContainsFunc(j.BucketHistory, func(known bucketSession) bool {
			return known.Node == session.Node && known.Generation == session.Generation && session.Epoch == 0
		}) {
			return v050.BucketObservation{}, errors.New("unadmitted historical writer observed during migration")
		}
	}
	members := make([]v050.BucketMember, 0, len(j.BucketHistory))
	for _, s := range j.BucketHistory {
		members = append(members, v050.BucketMember{Node: s.Node, Generation: s.Generation, SupersededBy: s.SupersededBy, Retired: true, Resolved: resolvedBucketSession(s)})
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return v050.BucketObservation{}, err
	}
	adapter, err := v050.New(j.RuntimeImage)
	if err != nil {
		return v050.BucketObservation{}, err
	}
	return adapter.InspectBucketMembership(ctx, reader, members, r.capacityNow)
}

func (r *Reconciler) migrationFailure(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object, cause error) (ctrl.Result, bool, error) {
	j.BucketMigration.SettledAt = time.Time{}
	if invalidated, ok := errors.AsType[*v050.BucketExpiryInvalidatedError](cause); ok {
		invalidateBucketExpiry([][]bucketSession{j.BucketHistory}, invalidated)
	}
	_, loss := errors.AsType[*v050.LossError](cause)
	if loss && j.Loss == "" {
		j.Loss = cause.Error()
	}
	// Persist first even if the old workload has already gone. No later phase may
	// forget a loss observation merely because no object remains to annotate.
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	if loss && w != nil {
		return r.recordLoss(ctx, f, w, res, j, j.Loss)
	}
	reason := "MigrationBlocked"
	if loss {
		reason = "PossibleDataLoss"
	}
	result, err := r.report(ctx, f, reason, cause.Error(), false)
	return result, true, err
}

// Creation itself is inert. Only the exact durably captured UID can receive a
// replica CAS, so an old leader's delayed Create cannot resurrect active compute.
func (r *Reconciler) activateMigratedBucket(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal) (ctrl.Result, bool, error) {
	m := j.BucketMigration
	target := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(f), target)
	if apierrors.IsNotFound(err) && !f.DeletionTimestamp.IsZero() {
		m.Phase = "Retained"
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	if err != nil {
		return ctrl.Result{}, true, err
	}
	expected := f.DeepCopy()
	expected.Spec.RuntimeImage = j.RuntimeImage
	expected.Spec.Replicas = 0
	if target.Annotations[operationKey] == m.ID+"/activate" {
		expected.Spec.Replicas = j.Applied
	}
	if m.TargetUID == "" || target.UID != m.TargetUID || target.Annotations[migrationKey] != m.ID || !matches(workload(expected, r.Options), target) {
		return ctrl.Result{}, true, errors.New("migration activation target changed")
	}
	if replicas(target) == 0 {
		if _, err := r.migrationRetirementEvidence(ctx, f, j); err != nil {
			return r.migrationFailure(ctx, f, res, j, nil, err)
		}
		if !f.DeletionTimestamp.IsZero() {
			uid, rv := target.UID, target.ResourceVersion
			if err := r.Delete(ctx, target, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, true, err
			}
			m.Phase = "Retained"
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		setReplicas(target, j.Applied)
		target.Annotations[operationKey] = m.ID + "/activate"
		if err := r.Update(ctx, target); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	m.Phase = "Complete"
	j.WorkloadUID = m.TargetUID
	resetMaintenanceCapacity(j)
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, true, nil
}
