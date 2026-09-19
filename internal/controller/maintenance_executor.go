package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Authorized is an irreversible exact-identity action. Pause and request changes
// stop admission of the next action, but cannot revoke an already authorized
// request from a previous leader. Recovery always retains that authority.
type maintenanceOperation struct {
	Coordinated              bool
	TargetReplicas           int32
	Persistent               []persistentMember
	ID, Kind, Token, Phase   string
	SourceImage, TargetImage string
	Targets                  []maintenanceTarget
	Index                    int
	Sessions                 []bucketSession
	SettledAt                time.Time
	Deadline                 time.Time
	StartedAt                time.Time
}
type maintenanceTarget struct {
	Name string
	UID  types.UID
}

func (r *Reconciler) beginMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	if w.GetUID() != j.WorkloadUID {
		return ctrl.Result{}, true, errors.New("maintenance workload changed")
	}
	if j.Loss != "" {
		result, err := r.report(ctx, f, "PossibleDataLoss", j.Loss, 0, false)
		return result, true, err
	}
	if j.Operation != nil {
		return ctrl.Result{}, false, nil
	}
	j.Maintenance = &maintenanceOperation{ID: j.Request.ID, Kind: j.Request.Kind, Token: j.Request.RestartToken, Phase: "Capture", StartedAt: r.capacityNow(), Deadline: r.capacityNow().Add(operationBudget)}
	return r.saveMaintenance(ctx, f, res, j)
}

func (r *Reconciler) executeMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	m := j.Maintenance
	save := func() (ctrl.Result, bool, error) {
		return r.saveMaintenance(ctx, f, res, j)
	}
	block := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		if e, ok := errors.AsType[*v050.BucketExpiryInvalidatedError](err); ok {
			for _, records := range [][]bucketSession{m.Sessions, j.BucketHistory} {
				for i := range records {
					if records[i].Node == e.Node && records[i].Generation == e.Generation {
						records[i].ExpiryObserved = false
						records[i].ExpiryInvalidated = true
					}
				}
			}
		}
		m.SettledAt = time.Time{}
		if saveErr := r.saveJournal(ctx, res, j); saveErr != nil {
			return ctrl.Result{}, true, saveErr
		}
		result, reportErr := r.report(ctx, f, "MaintenanceRecoveryBlocked", err.Error(), 0, false)
		return result, true, reportErr
	}
	if m.ID == "" || (m.Kind != "Restart" && m.Kind != "Delete" && m.Kind != "Contract" && m.Kind != "Upgrade") || m.Index < 0 || m.Index > len(m.Targets) || j.Operation != nil {
		return block(errors.New("invalid maintenance authority"))
	}
	if j.Loss != "" || w.GetAnnotations()[lossFenceKey] != "" {
		return block(errors.New("durable loss fence prohibits maintenance completion"))
	}
	expected := appliedRuntime(f, j)
	if m.Kind == "Upgrade" {
		image, err := transitionWorkloadImage(j, w)
		if err != nil {
			return block(err)
		}
		expected.Spec.RuntimeImage = image
	}
	expected.Spec.Replicas = j.Applied
	if m.Coordinated {
		expected.Spec.Replicas = coordinatedExpectedReplicas(m, j.Applied, w)
	}
	if m.Kind == "Delete" && (m.Phase == "Recovering" || m.Phase == "Cleanup" || (m.Phase == "Authorized" && w.GetAnnotations()[operationKey] == m.ID)) {
		expected.Spec.Replicas = 0
	}
	if w.GetUID() != j.WorkloadUID || !matches(workload(expected, r.Options), w) {
		return block(errors.New("maintenance workload identity or configuration changed"))
	}
	if w.GetAnnotations()[maintenanceFenceKey] != maintenanceFence(f) {
		if err := r.setMaintenanceFence(ctx, f, w, maintenanceFence(f)); err != nil {
			return block(err)
		}
	}
	if orderedBucket(f) {
		if err := r.scheduleOrderedBucket(ctx, f, j); err != nil {
			return block(err)
		}
	}
	if r.Evidence == nil {
		return block(errors.New("production recovery evidence unavailable"))
	}
	inventory, loss := r.Evidence.Observe(ctx, expected, j.Inventory)
	j.Inventory = inventory
	if loss != "" {
		return r.recordLoss(ctx, f, w, res, j, loss)
	}
	if m.Phase == "Capture" || m.Phase == "Next" {
		if paused(f) {
			result, err := r.report(ctx, f, "MaintenancePaused", "No new maintenance action admitted", 0, false)
			return result, true, err
		}
		if m.Kind == "Upgrade" && (!canStopUpgrade(f, j, r.Options) || runtimeImage(f) != m.TargetImage || !f.DeletionTimestamp.IsZero()) {
			j.Maintenance = nil
			return save()
		}
		if m.Kind == "Contract" && (f.Spec.Replicas != m.TargetReplicas || !f.DeletionTimestamp.IsZero() || !coordinatedDowntime(f)) {
			j.Maintenance = nil
			return save()
		}
		if m.Kind == "Restart" && (!f.DeletionTimestamp.IsZero() || runtimeImage(f) != j.RuntimeImage || f.Spec.Maintenance == nil || f.Spec.Maintenance.RestartToken != m.Token) {
			j.Maintenance = nil
			return save()
		}
		if !r.capacityNow().Before(m.Deadline) {
			return block(errors.New("maintenance admission deadline exceeded"))
		}
	}
	if f.Spec.Profile == "PersistentFleet" {
		beforeVersion := expected.ResourceVersion
		result, handled, err := r.executePersistentMaintenance(ctx, expected, res, j, w, block)
		if expected.ResourceVersion != beforeVersion {
			f.ResourceVersion, f.Status = expected.ResourceVersion, expected.Status
		}
		return result, handled, err
	}
	view := *j
	view.Operation = &lifecycleOperation{ID: m.ID, Phase: "Recovering", From: j.Applied, To: j.Applied, BucketCandidates: m.Sessions}
	if m.Kind == "Delete" {
		return r.executeBucketDeletion(ctx, f, res, j, w, block)
	}
	switch m.Phase {
	case "Capture", "Next":
		sessions, _, err := r.bucketAssessment(ctx, f, &view, j.Applied, false)
		if err != nil {
			return block(err)
		}
		if m.Phase == "Capture" {
			pods, err := r.Evidence.pods(ctx, f)
			if err != nil {
				return block(err)
			}
			for _, pod := range pods {
				m.Targets = append(m.Targets, maintenanceTarget{Name: pod.Name, UID: pod.UID})
			}
			slices.SortFunc(m.Targets, func(a, b maintenanceTarget) int {
				if a.Name < b.Name {
					return -1
				}
				if a.Name > b.Name {
					return 1
				}
				return 0
			})
		}
		m.Sessions = sessions
		if m.Index == len(m.Targets) {
			j.CompletedRestarts = append(j.CompletedRestarts, m.Token)
			j.History = append(j.History, lifecycleCompletion{ID: m.ID, From: j.Applied, To: j.Applied, EvidenceAt: r.capacityNow(), Outcome: "RestartComplete"})
			j.Maintenance = nil
			j.Request = nil
			return save()
		}
		target := m.Targets[m.Index]
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: target.Name}, pod); err != nil {
			return block(err)
		}
		if pod.UID != target.UID {
			return block(errors.New("restart target replaced outside admitted operation"))
		}
		candidates, err := r.Evidence.bucketCandidates(ctx, f, &view, r.Options)
		if err != nil {
			return block(err)
		}
		if err := validateRestartPlacement(f, candidates, target.UID); err != nil {
			return block(err)
		}
		if err := r.authorizeMaintenanceAction(ctx, w, m); err != nil {
			return block(err)
		}
		m.Phase = "Authorized"
		return save()
	case "Authorized":
		target := m.Targets[m.Index]
		pod := &corev1.Pod{}
		err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: target.Name}, pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return block(err)
		}
		if err == nil && pod.UID == target.UID {
			if m.Deadline.IsZero() || !r.capacityNow().Before(m.Deadline) {
				return block(errors.New("restart deadline expired before pod deletion"))
			}
			sessions, _, err := r.bucketAssessment(ctx, f, &view, j.Applied, false)
			if err != nil {
				return block(err)
			}
			if !slices.Equal(sessions, m.Sessions) {
				m.Sessions = sessions
				return save()
			}
			candidates, err := r.Evidence.bucketCandidates(ctx, f, &view, r.Options)
			if err != nil {
				return block(err)
			}
			if err := validateRestartPlacement(f, candidates, target.UID); err != nil {
				return block(err)
			}
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &target.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return block(err)
			}
		}
		m.Phase = "Recovering"
		return save()
	case "Recovering":
		sessions, at, err := r.bucketAssessment(ctx, f, &view, j.Applied, true)
		if err != nil {
			return block(err)
		}
		target := m.Targets[m.Index]
		if !slices.ContainsFunc(sessions, func(s bucketSession) bool { return s.Node == string(target.UID) && s.Retired && s.ExpiryObserved }) {
			return block(errors.New("restart target has not retired with positive lease expiry"))
		}
		m.Sessions = sessions
		if m.SettledAt.IsZero() {
			m.SettledAt = at
			return save()
		}
		if at.Sub(m.SettledAt) < 10*time.Second {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		j.BucketHistory = sessions
		m.Index++
		m.Phase = "Next"
		m.SettledAt = time.Time{}
		return save()
	default:
		return block(errors.New("unknown maintenance phase"))
	}
}

func (r *Reconciler) executeBucketDeletion(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object, block func(error) (ctrl.Result, bool, error)) (ctrl.Result, bool, error) {
	m := j.Maintenance
	save := func() (ctrl.Result, bool, error) {
		return r.saveMaintenance(ctx, f, res, j)
	}
	if f.DeletionTimestamp.IsZero() {
		return block(errors.New("deletion authority requires deleting fleet"))
	}
	switch m.Phase {
	case "Capture":
		view := *j
		view.Operation = &lifecycleOperation{ID: m.ID, Phase: "Recovering", From: j.Applied, To: j.Applied, BucketCandidates: m.Sessions}
		// Shutdown has no projected survivor placement/capacity requirement. Current
		// membership and no-log S3 obligations must nevertheless be fully assessed.
		sessions, _, err := r.bucketAssessment(ctx, f, &view, j.Applied, true)
		if err != nil {
			return block(err)
		}
		m.Sessions = sessions
		m.Phase = "Authorized"
		return save()
	case "Authorized":
		if replicas(w) != 0 {
			if replicas(w) != j.Applied || w.GetAnnotations()[maintenanceFenceKey] != "deleting" {
				return block(errors.New("deletion workload fence unavailable"))
			}
			setReplicas(w, 0)
			w.GetAnnotations()[operationKey] = m.ID
			if err := r.Update(ctx, w); err != nil {
				return ctrl.Result{}, true, err
			}
		} else if w.GetAnnotations()[operationKey] != m.ID {
			return block(errors.New("zero replicas lacks deletion authority"))
		}
		m.Phase = "Recovering"
		return save()
	case "Recovering":
		pods, err := r.Evidence.pods(ctx, f)
		if err != nil {
			return block(err)
		}
		if len(pods) != 0 {
			return block(errors.New("waiting for workload membership to become empty"))
		}
		members := make([]v050.BucketMember, 0, len(m.Sessions))
		for _, s := range m.Sessions {
			members = append(members, v050.BucketMember{Node: s.Node, Generation: s.Generation, SupersededBy: s.SupersededBy, Retired: true, Resolved: s.ExpiryObserved && !s.ExpiryInvalidated})
		}
		reader, err := r.Evidence.reader(ctx, f)
		if err != nil {
			return block(err)
		}
		adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
		if err != nil {
			return block(err)
		}
		evidence, err := adapter.InspectBucketMembership(ctx, reader, members, r.capacityNow)
		if err != nil {
			return block(err)
		}
		for i := range m.Sessions {
			m.Sessions[i].Retired = true
			m.Sessions[i].ExpiryObserved = true
			m.Sessions[i].ExpiryInvalidated = false
		}
		if m.SettledAt.IsZero() {
			m.SettledAt = evidence.ObservedAt
			return save()
		}
		if evidence.ObservedAt.Sub(m.SettledAt) < 10*time.Second {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		j.BucketHistory = m.Sessions
		m.Phase = "Cleanup"
		return save()
	case "Cleanup":
		uid := w.GetUID()
		if err := r.Delete(ctx, w, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, err
		}
		return r.completeRetainedDeletion(ctx, f, res, j)
	default:
		return block(errors.New("invalid shutdown phase"))
	}
}

func (r *Reconciler) completeRetainedDeletion(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal) (ctrl.Result, bool, error) {
	if res.Name != reservationName(f) {
		return ctrl.Result{}, true, errors.New("retained reservation identity changed")
	}
	// Permanent reservation, journal, PVCs, credentials and network isolation are
	// retained. No owner reference can cascade-delete data when the CR disappears.
	latest := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), latest); err != nil {
		return ctrl.Result{}, true, client.IgnoreNotFound(err)
	}
	if latest.UID != f.UID || latest.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, true, errors.New("fleet deletion identity changed")
	}
	if j.Maintenance == nil || j.Maintenance.Phase != "Cleanup" {
		return ctrl.Result{}, true, errors.New("missing final shutdown authority")
	}
	base := latest.DeepCopy()
	controllerutil.RemoveFinalizer(latest, Finalizer)
	return ctrl.Result{}, true, r.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func validateMaintenanceJournal(j *lifecycleJournal) error {
	m := j.Maintenance
	if m == nil {
		return nil
	}
	if j.Operation != nil || m.ID == "" || (m.Kind != "Restart" && m.Kind != "Delete" && m.Kind != "Contract" && m.Kind != "Upgrade") || m.Index < 0 || m.Index > len(m.Targets) {
		return errors.New("invalid maintenance journal")
	}
	switch m.Phase {
	case "Capture", "Next", "Stopping", "Authorized", "Recovering", "Cleanup", "Quiesced", "Empty", "Resuming":
	default:
		return errors.New("invalid maintenance phase")
	}
	if m.Coordinated {
		if (m.Kind != "Restart" && m.Kind != "Contract" && m.Kind != "Upgrade") || m.TargetReplicas < 1 || m.TargetReplicas > j.Applied || (m.Kind == "Contract" && (j.Applied != 2 || m.TargetReplicas != 1)) || ((m.Kind == "Restart" || m.Kind == "Upgrade") && m.TargetReplicas != j.Applied) {
			return errors.New("invalid coordinated maintenance authority")
		}
		if m.Phase != "Capture" && len(m.Persistent) != int(j.Applied) {
			return errors.New("coordinated capture must cover every applied member")
		}
		if m.Phase == "Quiesced" || m.Phase == "Empty" || m.Phase == "Resuming" {
			for _, member := range m.Persistent {
				if !member.Stopped || !member.RestartDenied {
					return errors.New("coordinated recovery lacks every exact stop receipt")
				}
			}
		}
	} else if m.Kind == "Contract" || m.Kind == "Upgrade" || m.Phase == "Quiesced" || m.Phase == "Empty" || m.Phase == "Resuming" {
		return errors.New("coordinated phase without authority")
	}
	if m.Kind == "Upgrade" {
		if !catalog.StoppedUpgrade(m.SourceImage, m.TargetImage) || j.RuntimeImage != m.SourceImage || !m.Coordinated || (m.Phase != "Capture" && m.Phase != "Stopping" && m.Phase != "Quiesced" && m.Phase != "Empty" && m.Phase != "Resuming") {
			return errors.New("invalid runtime transition authority")
		}
	} else if m.SourceImage != "" || m.TargetImage != "" {
		return errors.New("runtime transition fields on unrelated operation")
	}
	if m.Kind == "Restart" {
		if m.Token == "" || m.Phase == "Cleanup" {
			return errors.New("invalid restart authority")
		}
		if !m.Coordinated && (m.Phase == "Stopping" || m.Phase == "Authorized" || m.Phase == "Recovering") && m.Index == len(m.Targets) {
			return errors.New("restart target missing")
		}
	}
	seen := map[types.UID]bool{}
	for _, target := range m.Targets {
		if target.Name == "" || target.UID == "" || seen[target.UID] {
			return errors.New("ambiguous restart target inventory")
		}
		seen[target.UID] = true
	}
	return nil
}

// The workload CAS linearizes restart admission against pause/loss fences.
// An old leader must not admit a disruption after another leader acknowledged
// the fence. Only a successful journal write after this CAS permits the effect.
func (r *Reconciler) authorizeMaintenanceAction(ctx context.Context, w client.Object, m *maintenanceOperation) error {
	if w.GetAnnotations()[maintenanceFenceKey] != "" || w.GetAnnotations()[lossFenceKey] != "" {
		return errors.New("maintenance action fenced")
	}
	if w.GetAnnotations() == nil {
		w.SetAnnotations(map[string]string{})
	}
	key := fmt.Sprintf("%s/%d", m.ID, m.Index)
	w.GetAnnotations()["celld.eric.dev/maintenance-action"] = key
	return r.Update(ctx, w)
}

func (r *Reconciler) saveMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal) (ctrl.Result, bool, error) {
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	_, err := r.report(ctx, f, "LifecycleProgress", "Durable maintenance transition recorded; inspect lifecycle maintenance phase", 0, false)
	return ctrl.Result{RequeueAfter: time.Second}, true, err
}

func validateRestartPlacement(f *fleet.CelldFleet, candidates map[types.UID]bucketCandidate, target types.UID) error {
	if _, ok := candidates[target]; !ok {
		return errors.New("restart target absent from placement inventory")
	}
	survivors := maps.Clone(candidates)
	delete(survivors, target)
	return validateBucketPlacement(f, survivors, false)
}
