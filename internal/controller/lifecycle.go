package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const journalKey = "celld.example.com/lifecycle-journal"
const creationClaimsKey = "celld.example.com/creation-claim-uids"
const lossFenceKey = "celld.example.com/recovery-loss-fence"
const operationKey = "celld.example.com/lifecycle-operation"

// The reservation survives status loss and fleet deletion. Never prune session
// history or PVC identities automatically. Exhausting annotation capacity blocks.
type lifecycleJournal struct {
	Version          int
	Initial, Applied int32
	WorkloadUID      types.UID
	Operation        *lifecycleOperation
	Sessions         []v050.Session
	Claims           map[string]types.UID
	Loss             string
	History          []lifecycleCompletion
}
type lifecycleCompletion struct {
	ID, TargetPod, TargetUID, TargetGeneration string
	From, To                                   int32
	EvidenceAt                                 time.Time
}

type lifecycleOperation struct {
	ID, Phase, WorkloadVersion             string
	From, To                               int32
	TargetPod, TargetUID, TargetGeneration string
	Sessions                               []v050.Session
	SettledAt                              time.Time
}

// Qualification seam, deliberately unexported and not wired to a CLI flag.
// Capture must inventory all current AND historical sessions and map the highest
// ordinal to its pod UID and exact runtime generation. Validate must read fresh
// membership and /state for every survivor and verify retained PVC identities.
// Stopped must prove the selected process is fenced, never infer it from absence.
// A future production implementation needs independently qualified fencing.
type lifecycleEvidence interface {
	Capture(context.Context, *fleet.CelldFleet, int32, []v050.Session) (*lifecycleOperation, error)
	Validate(context.Context, *fleet.CelldFleet, *lifecycleOperation) error
	Stopped(context.Context, *fleet.CelldFleet, *lifecycleOperation) (bool, error)
	Reader(*fleet.CelldFleet) v050.Reader
	Now() time.Time
}

func replicas(w client.Object) int32 {
	switch w := w.(type) {
	case *appsv1.Deployment:
		if w.Spec.Replicas == nil {
			return -1
		}
		return *w.Spec.Replicas
	case *appsv1.StatefulSet:
		if w.Spec.Replicas == nil {
			return -1
		}
		return *w.Spec.Replicas
	default:
		panic("unsupported workload")
	}
}
func setReplicas(w client.Object, n int32) {
	switch w := w.(type) {
	case *appsv1.Deployment:
		w.Spec.Replicas = new(n)
	case *appsv1.StatefulSet:
		w.Spec.Replicas = new(n)
	}
}
func readJournal(res *fleet.CelldStorageReservation) (*lifecycleJournal, error) {
	raw := res.Annotations[journalKey]
	if raw == "" {
		return nil, nil
	}
	var j lifecycleJournal
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return nil, err
	}
	if j.Version != 1 || j.Initial < 1 || j.Initial > 100 || j.Applied < 1 || j.Applied > 100 {
		return nil, errors.New("invalid lifecycle journal")
	}
	if op := j.Operation; op != nil {
		if op.ID == "" || op.From != j.Applied || op.To < 1 || op.To > 100 || op.To == op.From || (op.To < op.From && op.To != op.From-1) || (op.Phase != "Intent" && op.Phase != "Prepared" && op.Phase != "Recovering") {
			return nil, errors.New("invalid lifecycle operation")
		}
	}
	if j.Claims == nil {
		j.Claims = map[string]types.UID{}
	}
	return &j, nil
}
func (r *Reconciler) saveJournal(ctx context.Context, res *fleet.CelldStorageReservation, j *lifecycleJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if res.Annotations == nil {
		res.Annotations = map[string]string{}
	}
	res.Annotations[journalKey] = string(b)
	// Update carries the read resourceVersion. Never retry with a fresh version:
	// another controller may already have advanced the operation.
	return r.Update(ctx, res)
}
func (r *Reconciler) reservationMatches(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, want fleet.ReservationSpec) bool {
	j, err := readJournal(res)
	if err != nil {
		return false
	}
	baseline := f.DeepCopy()
	want.InitialReplicas = res.Spec.InitialReplicas
	switch {
	case res.Spec.InitialReplicas != 0:
		if res.Spec.InitialReplicas < 1 || res.Spec.InitialReplicas > 100 || (j != nil && j.Initial != res.Spec.InitialReplicas) {
			return false
		}
		baseline.Spec.Replicas = res.Spec.InitialReplicas
	case j != nil:
		baseline.Spec.Replicas = j.Initial
	case res.Annotations[attemptAnnotation] != "":
		w := emptyObject(workload(f, r.Options))
		if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err == nil {
			baseline.Spec.Replicas = replicas(w)
		}
	}
	want.SpecHash = specHash(baseline)
	return res.Spec == want
}

func (r *Reconciler) lifecycle(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, w client.Object) (ctrl.Result, bool, error) {
	block := func(reason, message string) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, reason, message, 0, false)
		return result, true, err
	}
	progress := func(message string) (ctrl.Result, bool, error) { return block("LifecycleProgress", message) }
	save := func(j *lifecycleJournal) (ctrl.Result, bool, error) {
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		return progress("Durable lifecycle transition recorded; inspect status.lifecycle for the operation and target")
	}
	j, err := readJournal(res)
	if err != nil {
		return block("JournalInvalid", err.Error())
	}
	if res.Annotations[attemptAnnotation] == "" {
		return block("LifecycleBlocked", "Missing creation journal; refusing workload adoption")
	}
	if j == nil {
		baseline := f.DeepCopy()
		baseline.Spec.Replicas = replicas(w)
		if !matches(workload(baseline, r.Options), w) || w.GetAnnotations()[operationKey] != "" {
			return block("LifecycleBlocked", "Workload drift or lost lifecycle journal; manual investigation required")
		}
		initial := res.Spec.InitialReplicas
		if initial == 0 {
			initial = replicas(w)
		}
		j = &lifecycleJournal{Version: 1, Initial: initial, Applied: replicas(w), WorkloadUID: w.GetUID(), Claims: map[string]types.UID{}}
		// Creation records claim UIDs before workload creation. Verify those bindings;
		// missing or replaced claims can never be adopted.
		var created map[string]types.UID
		if f.Spec.Profile == "PersistentFleet" {
			if err := json.Unmarshal([]byte(res.Annotations[creationClaimsKey]), &created); err != nil {
				return block("StorageIdentityConflict", "Missing creation PVC UID inventory; retained disks require manual review")
			}
		}
		for _, claim := range initialClaims(baseline, w) {
			got := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(claim), got); err != nil {
				return block("StorageIdentityConflict", err.Error())
			}
			createdUID, recorded := created[got.Name]
			if !recorded || createdUID != got.UID || got.Labels[FleetLabel] != string(f.UID) || got.Annotations["celld.example.com/storage-reservation"] != res.Name || !got.DeletionTimestamp.IsZero() {
				return block("StorageIdentityConflict", "Initial retained claim binding changed")
			}
			j.Claims[got.Name] = got.UID
		}
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	if j.WorkloadUID != w.GetUID() {
		return block("LifecycleBlocked", "Workload UID changed; refusing replacement adoption")
	}
	expectedCount := j.Applied
	op := j.Operation
	if op != nil && w.GetAnnotations()[operationKey] == op.ID {
		expectedCount = op.To
	}
	expected := f.DeepCopy()
	expected.Spec.Replicas = expectedCount
	if !matches(workload(expected, r.Options), w) {
		return block("LifecycleBlocked", "Workload differs from journaled infrastructure; no template mutation or replica drift repair is allowed")
	}
	// The workload fence is authoritative even if a leader crashed before the
	// reservation journal could record the loss. It also invalidates old replica CASes.
	if loss := w.GetAnnotations()[lossFenceKey]; loss != "" && j.Loss == "" {
		j.Loss = loss
		return save(j)
	}
	for name, uid := range j.Claims {
		got := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, got); err != nil {
			return block("StorageIdentityConflict", err.Error())
		}
		if got.UID != uid || !got.DeletionTimestamp.IsZero() {
			return block("StorageIdentityConflict", "Retained PVC missing, replaced or deleting: "+name)
		}
	}
	if op == nil {
		if f.Spec.Replicas == j.Applied {
			return ctrl.Result{}, false, nil
		}
		op = &lifecycleOperation{ID: string(uuid.NewUUID()), Phase: "Intent", From: j.Applied, To: f.Spec.Replicas, WorkloadVersion: w.GetResourceVersion()}
		if op.To < op.From {
			if f.Spec.Profile == "Bucket" {
				return block("BucketCompletionUnqualified", "No qualified no-log completion rule or deterministic Deployment victim; contraction is unavailable")
			}
			if !r.Options.LocalTest || r.localLifecycle == nil {
				return block("FencingUnqualified", "Real process fencing and AWS recovery are unqualified; PersistentFleet contraction is unavailable")
			}
			if j.Loss != "" {
				return block("PossibleDataLoss", j.Loss)
			}
			captured, err := r.localLifecycle.Capture(ctx, f, op.From-1, j.Sessions)
			if err != nil {
				return block("RecoveryBlocked", err.Error())
			}
			if captured == nil {
				return block("RecoveryBlocked", "No target/session capture")
			}
			captured.ID, captured.Phase, captured.From, captured.To, captured.WorkloadVersion = op.ID, "Intent", op.From, op.From-1, op.WorkloadVersion
			op = captured
			for _, previous := range j.Sessions {
				if !slices.Contains(op.Sessions, previous) {
					return block("RecoveryBlocked", "Historical session was omitted or changed")
				}
			}
			if op.TargetPod != fmt.Sprintf("%s-%d", f.Name, op.To) || op.TargetUID == "" || op.TargetGeneration == "" || len(op.Sessions) == 0 {
				return block("RecoveryBlocked", "Incomplete selected target/session inventory")
			}
		}
		if op.To < op.From {
			selected := false
			for _, session := range op.Sessions {
				if session.Node == op.TargetPod && session.Generation == op.TargetGeneration && session.Epoch > 0 && !session.Stopped {
					selected = true
				}
			}
			if !selected {
				return block("RecoveryBlocked", "Selected target lacks a live exact session")
			}
		}
		j.Operation = op
		return save(j) // No external action before the intent has survived an API write.
	}
	if (op.Phase == "Intent" || op.Phase == "Prepared") && replicas(w) == op.From && w.GetResourceVersion() != op.WorkloadVersion {
		// An obsolete version cannot still win a CAS. Persist the replacement
		// version, then revalidate on another reconcile before attempting any effect.
		op.WorkloadVersion = w.GetResourceVersion()
		return save(j)
	}
	if op.To > op.From {
		return r.expand(ctx, f, res, j, w)
	}
	if !r.Options.LocalTest || r.localLifecycle == nil {
		return block("FencingUnqualified", "Operation retained; qualified process fencing is unavailable")
	}
	if j.Loss != "" {
		return block("PossibleDataLoss", j.Loss)
	}
	if op.Phase == "Intent" && replicas(w) == op.To && w.GetAnnotations()[operationKey] == op.ID {
		op.Phase = "Recovering"
		return save(j)
	}
	if op.Phase == "Intent" {
		// Desired count changes never retarget an existing operation. Before issue,
		// freeze rather than cancel: a delayed previous leader could still issue it.
		if f.Spec.Replicas >= op.From {
			return block("DesiredChanged", "Existing removal intent retained; no new removal authorized")
		}
		if err := r.localLifecycle.Validate(ctx, f, op); err != nil {
			return block("RecoveryBlocked", err.Error())
		}
		adapter, err := v050.New(Image)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		evidence, err := adapter.Inspect(ctx, r.localLifecycle.Reader(f), v050.Request{OperationID: op.ID, Sessions: op.Sessions, InventoryComplete: true, CapturedAt: r.localLifecycle.Now(), MaxAge: 5 * time.Second, PageBudget: 1000}, r.localLifecycle.Now)
		if err != nil {
			if _, ok := errors.AsType[*v050.LossError](err); ok {
				return r.recordLoss(ctx, f, w, res, j, err.Error())
			}
			return block("RecoveryBlocked", err.Error())
		}
		if err := r.localLifecycle.Validate(ctx, f, op); err != nil {
			return block("RecoveryBlocked", err.Error())
		}
		if !evidenceFresh(evidence, r.localLifecycle.Now()) {
			return block("RecoveryBlocked", "Preflight evidence expired during membership revalidation")
		}
		if err := r.applyReplicas(ctx, w, op); err != nil {
			return block("ReplicaUpdateBlocked", err.Error())
		}
		op.Phase = "Recovering"
		return save(j)
	}
	recoveryBlock := func(reason, message string) (ctrl.Result, bool, error) {
		if !op.SettledAt.IsZero() {
			op.SettledAt = time.Time{}
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		return block(reason, message)
	}
	if err := r.localLifecycle.Validate(ctx, f, op); err != nil {
		return recoveryBlock("RecoveryBlocked", err.Error())
	}
	stopped, err := r.localLifecycle.Stopped(ctx, f, op)
	if err != nil || !stopped {
		return recoveryBlock("FencingRequired", "Exact selected process termination is unconfirmed; retain disk and investigate fencing")
	}
	sessions := append([]v050.Session(nil), op.Sessions...)
	found := false
	for i := range sessions {
		if sessions[i].Node == op.TargetPod && sessions[i].Generation == op.TargetGeneration {
			sessions[i].Stopped = true
			found = true
		}
	}
	if !found {
		return block("RecoveryBlocked", "Selected runtime session is absent from durable inventory")
	}
	adapter, err := v050.New(Image)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	evidence, err := adapter.Assess(ctx, r.localLifecycle.Reader(f), v050.Request{OperationID: op.ID, Sessions: sessions, InventoryComplete: true, CapturedAt: r.localLifecycle.Now(), MaxAge: 5 * time.Second, PageBudget: 1000}, r.localLifecycle.Now)
	if err != nil {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		op.SettledAt = time.Time{} // uncertainty restarts settling, never completes it
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		return block("RecoveryBlocked", err.Error())
	}
	if err := r.localLifecycle.Validate(ctx, f, op); err != nil {
		return recoveryBlock("RecoveryBlocked", err.Error())
	}
	if !evidenceFresh(evidence, r.localLifecycle.Now()) {
		return recoveryBlock("RecoveryBlocked", "Recovery evidence expired during membership revalidation")
	}
	if op.SettledAt.IsZero() {
		op.SettledAt = evidence.ObservedAt
		return save(j)
	}
	if evidence.ObservedAt.Sub(op.SettledAt) < 10*time.Second {
		return progress("Recovery evidence revalidated; waiting for another full assessment after settling")
	}
	j.History = append(j.History, completion(op, evidence.ObservedAt))
	j.Sessions, j.Applied, j.Operation = sessions, op.To, nil
	return save(j)
}

// A delayed old leader can only replay this exact CAS once. Never fetch a fresh
// resourceVersion and retry an old intent: that could remove a second target.
func (r *Reconciler) applyReplicas(ctx context.Context, w client.Object, op *lifecycleOperation) error {
	if op.To < op.From && w.GetAnnotations()[lossFenceKey] != "" {
		return errors.New("workload carries a durable recovery loss fence")
	}
	if replicas(w) == op.To && w.GetAnnotations()[operationKey] == op.ID {
		return nil
	}
	if replicas(w) != op.From || w.GetResourceVersion() != op.WorkloadVersion {
		return errors.New("workload changed since durable intent; operation is frozen for investigation")
	}
	setReplicas(w, op.To)
	if w.GetAnnotations() == nil {
		w.SetAnnotations(map[string]string{})
	}
	w.GetAnnotations()[operationKey] = op.ID
	return r.Update(ctx, w)
}

func (r *Reconciler) expand(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	fail := func(err error) (ctrl.Result, bool, error) {
		result, reportErr := r.report(ctx, f, "ScaleOutBlocked", err.Error(), 0, false)
		return result, true, reportErr
	}
	if op.Phase == "Intent" {
		for ordinal := op.From; ordinal < op.To; ordinal++ {
			node := fmt.Sprintf("%s-%d", f.Name, ordinal)
			for _, session := range j.Sessions {
				if session.Node == node {
					return fail(errors.New("retained runtime identity reactivation is unqualified: " + node))
				}
			}
		}
		target := f.DeepCopy()
		target.Spec.Replicas = op.To
		claims := initialClaims(target, workload(target, r.Options))
		for _, claim := range claims {
			if _, known := j.Claims[claim.Name]; known {
				continue
			}
			// Intent already exists. Atomic Create excludes pre-existing retained disks.
			// A crash after Create before UID persistence deliberately blocks, preserving
			// the disk instead of guessing whether it is ours.
			if err := r.Create(ctx, claim); err != nil {
				return fail(fmt.Errorf("exclusive PVC allocation %s: %w", claim.Name, err))
			}
			j.Claims[claim.Name] = claim.UID
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		op.Phase = "Prepared"
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
	}
	if err := r.applyReplicas(ctx, w, op); err != nil {
		return fail(err)
	}
	j.History = append(j.History, completion(op, time.Time{}))
	j.Applied, j.Operation = op.To, nil
	return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
}

func evidenceFresh(evidence v050.Evidence, now time.Time) bool {
	return !evidence.ObservedAt.IsZero() && !evidence.ObservedAt.After(now) && now.Sub(evidence.ObservedAt) <= 5*time.Second
}

func completion(op *lifecycleOperation, evidenceAt time.Time) lifecycleCompletion {
	return lifecycleCompletion{ID: op.ID, TargetPod: op.TargetPod, TargetUID: op.TargetUID, TargetGeneration: op.TargetGeneration, From: op.From, To: op.To, EvidenceAt: evidenceAt}
}

// recordLoss linearizes a negative recovery finding on the same Kubernetes object
// as replica issuance. A stale issuer cannot pass its workload CAS after this
// update. If issuance won first, that removal was already issued; this fence still
// prohibits all subsequent contractions. Never refresh an issuer's RV here.
func (r *Reconciler) recordLoss(ctx context.Context, f *fleet.CelldFleet, w client.Object, res *fleet.CelldStorageReservation, j *lifecycleJournal, message string) (ctrl.Result, bool, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := emptyObject(w)
		if err := r.Get(ctx, client.ObjectKeyFromObject(w), current); err != nil {
			return err
		}
		if current.GetUID() != j.WorkloadUID {
			return errors.New("workload identity changed while fencing loss")
		}
		if existing := current.GetAnnotations()[lossFenceKey]; existing != "" {
			message = existing
			return nil
		}
		if current.GetAnnotations() == nil {
			current.SetAnnotations(map[string]string{})
		}
		current.GetAnnotations()[lossFenceKey] = message
		return r.Update(ctx, current)
	})
	if err != nil {
		return ctrl.Result{}, true, err
	}
	j.Loss = message
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	result, err := r.report(ctx, f, "PossibleDataLoss", message, 0, false)
	return result, true, err
}
