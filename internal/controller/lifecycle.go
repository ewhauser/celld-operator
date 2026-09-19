package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/runtime/catalog"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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
// history or PVC identities automatically. Large journals use immutable archives.
type lifecycleJournal struct {
	InfrastructureFences []infrastructureFence
	BucketMigration      *bucketMigration
	Maintenance          *maintenanceOperation
	CompletedRestarts    []string
	Inventory            recoveryInventory
	Request              *disruptionRequest
	RuntimeImage         string
	Version              int
	Initial, Applied     int32
	WorkloadUID          types.UID
	Operation            *lifecycleOperation
	Sessions             []v050.Session
	Claims               map[string]types.UID
	Loss                 string
	History              []lifecycleCompletion
	Capacity             *capacity.State
	BucketHistory        []bucketSession
	PersistentHistory    []persistentMember
}
type lifecycleCompletion struct {
	ID, TargetPod, TargetUID, TargetGeneration string
	From, To                                   int32
	EvidenceAt                                 time.Time
	Outcome                                    string
}

type lifecycleOperation struct {
	StartedAt, Deadline                    time.Time
	Stalled                                bool
	PolicyHash                             string
	ManualBaseline                         int32
	Automatic                              bool
	ID, Phase, WorkloadVersion             string
	From, To                               int32
	TargetPod, TargetUID, TargetGeneration string
	Sessions                               []v050.Session
	BucketCandidates                       []bucketSession
	PersistentMembers                      []persistentMember
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
	if j.Version == 1 && j.RuntimeImage == "" {
		j.RuntimeImage = Image
	} // Legacy journals used only this pin.
	if !knownRuntime(j.RuntimeImage) {
		return nil, errors.New("unsupported journal runtime image")
	}
	if (j.Version != 1 && j.Version != 2 && j.Version != 3 && j.Version != 4 && j.Version != 5 && j.Version != 6 && j.Version != 7 && j.Version != 8) || j.Initial < 1 || j.Initial > 100 || j.Applied < 1 || j.Applied > 100 {
		return nil, errors.New("invalid lifecycle journal")
	}
	// Older binaries reject version 8 instead of ignoring handoff, maintenance, and session succession authority.
	j.Version = 8
	if req := j.Request; req != nil {
		if req.ID == "" || req.SourceImage != j.RuntimeImage || req.WorkloadUID != j.WorkloadUID || (req.Kind != "Upgrade" && req.Kind != "Restart" && req.Kind != "Delete") {
			return nil, errors.New("invalid disruption request")
		}
	}
	if op := j.Operation; op != nil {
		if !op.StartedAt.IsZero() && !op.Deadline.IsZero() && !op.Deadline.After(op.StartedAt) {
			return nil, errors.New("invalid operation deadline")
		}
		if (op.Automatic && (j.Capacity == nil || op.PolicyHash == "" || op.ManualBaseline < 1 || op.ManualBaseline > 100)) || op.ID == "" || op.From != j.Applied || op.To < 1 || op.To > 100 || op.To == op.From || (op.To < op.From && op.To != op.From-1) || (op.Phase != "Intent" && op.Phase != "Prepared" && op.Phase != "Recovering" && op.Phase != "Blocked" && op.Phase != "Canceling" && op.Phase != "Stopping" && op.Phase != "Retiring" && op.Phase != "Reactivating") {
			return nil, errors.New("invalid lifecycle operation")
		}
	}
	if j.Claims == nil {
		j.Claims = map[string]types.UID{}
	}
	if err := validateBucketMigration(&j); err != nil {
		return nil, err
	}
	if err := validateMaintenanceJournal(&j); err != nil {
		return nil, err
	}
	if err := validatePersistentJournal(&j); err != nil {
		return nil, err
	}
	return &j, nil
}
func (r *Reconciler) saveJournal(ctx context.Context, res *fleet.CelldStorageReservation, j *lifecycleJournal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if res.Annotations == nil {
		res.Annotations = map[string]string{}
	}
	if len(b) > 180*1024 {
		b, err = r.archiveJournal(ctx, res, b)
		if err != nil {
			return err
		}
	}
	res.Annotations[journalKey] = string(b)
	// Update carries the read resourceVersion. Never retry with a fresh version:
	// another controller may already have advanced the operation.
	return r.Update(ctx, res)
}
func (r *Reconciler) reservationMatches(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, want fleet.ReservationSpec) bool {
	j, err := r.loadJournal(ctx, res)
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
	if j != nil && j.BucketMigration != nil {
		baseline.Spec.BucketWorkload = "Deployment"
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
	j, err := r.loadJournal(ctx, res)
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
		j = &lifecycleJournal{Version: 8, RuntimeImage: runtimeImage(f), Initial: initial, Applied: replicas(w), WorkloadUID: w.GetUID(), Claims: map[string]types.UID{}}
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
		if got.UID != uid || !got.DeletionTimestamp.IsZero() || len(got.OwnerReferences) != 0 || got.Labels[FleetLabel] != string(f.UID) || got.Annotations["celld.example.com/storage-reservation"] != res.Name {
			return block("StorageIdentityConflict", "Retained PVC identity, ownership or deletion state changed: "+name)
		}
	}
	if j.Maintenance != nil {
		return r.executeMaintenance(ctx, f, res, j, w)
	}
	expectedCount := j.Applied
	op := j.Operation
	if op != nil && w.GetAnnotations()[operationKey] == op.ID {
		expectedCount = op.To
	}
	expected := f.DeepCopy()
	expected.Spec.Replicas = expectedCount
	expected.Spec.RuntimeImage = j.RuntimeImage
	if !matches(workload(expected, r.Options), w) {
		return block("LifecycleBlocked", "Workload differs from journaled infrastructure; no template mutation or replica drift repair is allowed")
	}
	if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "" {
		if err := r.schedulePersistent(ctx, appliedRuntime(f, j), j); err != nil {
			if _, loss := errors.AsType[*v050.LossError](err); loss {
				return r.recordLoss(ctx, f, w, res, j, err.Error())
			}
			return block("PersistentSchedulingBlocked", err.Error())
		}
	}
	if orderedBucket(f) {
		if err := r.scheduleOrderedBucket(ctx, f, j); err != nil {
			return block("BucketSchedulingBlocked", err.Error())
		}
	}
	if r.Evidence != nil {
		inventory, loss := r.Evidence.Observe(ctx, appliedRuntime(f, j), j.Inventory)
		j.Inventory = inventory
		if loss != "" && j.Loss == "" {
			return r.recordLoss(ctx, f, w, res, j, "possible loss declaration: "+loss)
		}
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	if maintenanceFence(f) == "" && w.GetAnnotations()[maintenanceFenceKey] != "" {
		// A crash may have left only the workload fence. Reset history durably
		// before releasing it, including when pause ended during that crash.
		resetMaintenanceCapacity(j)
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}

		if err := r.setMaintenanceFence(ctx, f, w, ""); err != nil {
			return ctrl.Result{}, true, err
		}
		return progress("Maintenance fence released; revalidate the recorded operation on the next reconcile")
	}
	if maintenanceFence(f) != "" {
		if !f.DeletionTimestamp.IsZero() {
			_, _, err := r.persistDisruptionRequest(ctx, f, res, j, w)
			if err != nil {
				return ctrl.Result{}, true, err
			}
		}
		resetMaintenanceCapacity(j)
		issued := op != nil && w.GetAnnotations()[operationKey] == op.ID && replicas(w) == op.To
		if !issued {
			if op == nil && !f.DeletionTimestamp.IsZero() {
				return r.disruption(ctx, f, res, j, w)
			}
			if op != nil {
				op.SettledAt = time.Time{}
			}

			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{}, false, nil
		}
		if op.Phase == "Reactivating" || (f.Spec.Profile == "PersistentFleet" && op.To > op.From && slices.ContainsFunc(j.PersistentHistory, func(m persistentMember) bool {
			return m.Retired && reactivatedNode(f.Name, m.Node, op.From, op.To)
		})) {
			// Replica issuance can survive a crash before Reactivating is saved.
			// Preserve that authority too; pause cannot certify volume reuse.
			return ctrl.Result{}, false, nil
		}
		if op.To > op.From {
			j.History = append(j.History, completion(op, time.Time{}))
			j.Applied, j.Operation = op.To, nil
			return save(j)
		}
	}
	if op == nil {
		if f.Spec.Profile == "Bucket" && r.Evidence != nil {
			changed, admissionErr := r.admitBucketHistory(ctx, f, j)
			if _, loss := errors.AsType[*v050.LossError](admissionErr); loss {
				return r.recordLoss(ctx, f, w, res, j, admissionErr.Error())
			}
			if changed {
				if err := r.saveJournal(ctx, res, j); err != nil {
					return ctrl.Result{}, true, err
				}
			}
			// Incomplete/uncertain observational admission must not prevent
			// previously supported additive capacity. Removal still revalidates
			// all history independently and cannot use missing admission.
		}
		if result, handled, err := r.disruption(ctx, f, res, j, w); handled || err != nil {
			return result, handled, err
		}
		target, automatic := r.capacityTarget(ctx, f, j)
		// Persist diagnostics even when a qualification gate will reject the request.
		// No replica action occurs until the later atomic journal+intent write succeeds.
		if j.Capacity != nil {
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		if target == j.Applied {
			return ctrl.Result{}, false, nil
		}
		if automatic {
			latest := &fleet.CelldFleet{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), latest); err != nil {
				return ctrl.Result{}, true, err
			}
			latest.Default()
			if latest.UID != f.UID || !latest.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(latest.Spec, f.Spec) {
				return block("CapacityChanged", "Fleet changed during collection; discard the recommendation and reconcile the new request")
			}
		}
		op = &lifecycleOperation{ID: string(uuid.NewUUID()), Phase: "Intent", StartedAt: r.capacityNow(), Deadline: r.capacityNow().Add(operationBudget), From: j.Applied, To: target, WorkloadVersion: w.GetResourceVersion(), Automatic: automatic}
		if op.To < op.From && (f.Spec.Profile == "Bucket" || (f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "")) && r.Evidence != nil {
			op.To = op.From - 1
			op.Phase = "Blocked"
			if automatic {
				op.PolicyHash = j.Capacity.Config
				op.ManualBaseline = f.Spec.Replicas
			}
			j.Operation = op
			return save(j)
		}
		if op.To < op.From && (!r.Options.LocalTest || r.localLifecycle == nil) {
			// Record blocked authority without pretending target/session evidence is
			// qualified. No production execution path accepts this phase.
			op.Phase, op.To = "Blocked", op.From-1
			if f.Spec.Profile == "PersistentFleet" {
				op.TargetPod = fmt.Sprintf("%s-%d", f.Name, op.To)
			}
			if automatic {
				op.PolicyHash = j.Capacity.Config
				op.ManualBaseline = f.Spec.Replicas
			}
			j.Operation = op
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			reason, message := r.productionRemovalBlock(ctx, f, j)
			if reason == "PossibleDataLoss" && j.Loss == "" {
				return r.recordLoss(ctx, f, w, res, j, message)
			}
			return block(reason, message)
		}
		if op.To < op.From {
			if f.Spec.Profile == "Bucket" {
				return block("BucketCompletionUnqualified", "Bucket completion authority is unqualified; every possible Deployment victim must pass preflight")
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
			captured.Automatic = automatic
			captured.StartedAt, captured.Deadline = op.StartedAt, op.Deadline
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
		if automatic {
			op.PolicyHash = j.Capacity.Config
			op.ManualBaseline = f.Spec.Replicas
		}
		j.Operation = op
		if j.Capacity != nil {
			capacity.RecordAction(j.Capacity, r.capacityNow(), op.From, op.To)
		}
		return save(j) // No external action before the intent has survived an API write.
	}
	if op.StartedAt.IsZero() || op.Deadline.IsZero() {
		// Legacy in-flight records get one durable deadline on first resumption.
		op.StartedAt = r.capacityNow()
		op.Deadline = op.StartedAt.Add(operationBudget)
		return save(j)
	}
	if op.Phase == "Canceling" {
		return r.cancelRemoval(ctx, res, j, w)
	}
	if op.To < op.From && (op.Phase == "Intent" || op.Phase == "Blocked") && maintenanceFence(f) == "" {
		target, _ := r.capacityTarget(ctx, f, j)
		if target > op.From {
			op.Phase = "Canceling"
			return save(j)
		}
	}
	if !r.capacityNow().Before(op.Deadline) && !op.Stalled {
		op.Stalled = true
		return save(j)
	}
	if f.Spec.Profile == "Bucket" && op.To < op.From && r.Evidence != nil {
		// capacityTarget above refreshes an unissued removal's stabilization
		// history. Persist negative observations too, before any executor return;
		// a restart must not revive the earlier qualified low-demand window.
		if j.Capacity != nil {
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		return r.contractBucket(ctx, f, res, j, w)
	}
	if f.Spec.Profile == "PersistentFleet" && op.To < op.From && r.Options.LauncherImage != "" && r.Evidence != nil {
		// Persist negative stabilization observations even when preflight fails.
		if j.Capacity != nil {
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		if op.From == 2 && op.To == 1 && !op.Automatic && (op.Phase == "Blocked" || op.Phase == "Intent") && coordinatedDowntime(f) {
			return r.beginCoordinatedContraction(ctx, f, res, j, w)
		}
		return r.contractPersistent(ctx, f, res, j, w)
	}
	if op.Phase == "Blocked" {
		if op.Stalled {
			return block("OperationStalled", "Removal deadline exceeded; authority and evidence retained; compatible additions can supersede this unissued request")
		}
		reason, message := r.productionRemovalBlock(ctx, f, j)
		if reason == "PossibleDataLoss" && j.Loss == "" {
			return r.recordLoss(ctx, f, w, res, j, message)
		}
		return block(reason, message)
	}
	// Expiration never certifies recovery or cancels an issued operation. Prevent
	// further unissued effects, while still reconstructing an already issued CAS.
	if op.Stalled && (w.GetAnnotations()[operationKey] != op.ID || replicas(w) != op.To) && op.Phase != "Recovering" {
		return block("OperationStalled", "Operation deadline exceeded before replica issuance; authority retained")
	}
	if maintenanceFence(f) == "" && f.Spec.Capacity != nil && j.Capacity != nil {
		observation := capacity.Observation{At: r.capacityNow()}
		if r.Collector != nil {
			observation = r.Collector.Collect(ctx, f)
		}
		*j.Capacity = capacity.Evaluate(*f.Spec.Capacity, *j.Capacity, observation, max(op.From, op.To))
		if j.Capacity.Decision.Reason != "PendingCapacity" && j.Capacity.Decision.Reason != "IneffectiveCapacity" {
			j.Capacity.Decision.Reason = "LifecycleInFlight"
			j.Capacity.Decision.Message = "Recorded operation retains its original target; new policy and manual requests wait"
		}
		j.Capacity.Decision.DesiredReplicas = op.To
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
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
		if !op.Automatic && f.Spec.Replicas >= op.From {
			return block("DesiredChanged", "Existing removal intent retained; no new removal authorized")
		}
		if err := r.localLifecycle.Validate(ctx, f, op); err != nil {
			return block("RecoveryBlocked", err.Error())
		}
		adapter, err := catalog.New(j.RuntimeImage)
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
		if op.Automatic {
			if f.Spec.Capacity == nil || f.Spec.Capacity.Mode != "Automatic" || op.PolicyHash != j.Capacity.Config || op.ManualBaseline != f.Spec.Replicas || op.To < f.Spec.Capacity.MinReplicas || r.Collector == nil {
				return block("CapacityChanged", "Automatic removal intent retained; changed policy/manual request requires review")
			}
			observation := r.Collector.Collect(ctx, f)
			if j.Capacity.LowSince.IsZero() || j.Capacity.LowSamples < f.Spec.Capacity.MinSamples || observation.At.Sub(j.Capacity.LowSince) < capacity.Seconds(f.Spec.Capacity.ScaleInStabilizationSeconds) || !capacity.LowDemand(*f.Spec.Capacity, observation, op.From) || observation.At.After(r.capacityNow()) || r.capacityNow().Sub(observation.At) > capacity.Seconds(f.Spec.Capacity.MaxAgeSeconds) {
				return block("CapacityUncertain", "Fresh complete low-demand evidence is required before automatic removal")
			}
			if !evidenceFresh(evidence, r.localLifecycle.Now()) {
				return block("RecoveryBlocked", "Recovery evidence expired during capacity revalidation")
			}
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
	adapter, err := catalog.New(j.RuntimeImage)
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
	if w.GetAnnotations()[canceledOperationKey] == op.ID {
		return errors.New("operation was canceled on the workload")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.GetAnnotations()[maintenanceFenceKey] != "" {
		return errors.New("workload carries a maintenance fence")
	}
	if op.To < op.From && w.GetAnnotations()[lossFenceKey] != "" {
		return errors.New("workload carries a durable recovery loss fence")
	}
	if replicas(w) == op.To && w.GetAnnotations()[operationKey] == op.ID {
		return nil
	}
	if op.Phase == "Blocked" || op.Phase == "Canceling" {
		return errors.New("operation phase cannot issue replicas")
	}
	if !op.Deadline.IsZero() && !r.capacityNow().Before(op.Deadline) {
		return errors.New("operation deadline expired")
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
	if op.Phase == "Reactivating" {
		return r.finishReactivation(ctx, f, res, j, w)
	}
	if op.Phase == "Intent" {
		for ordinal := op.From; ordinal < op.To; ordinal++ {
			node := fmt.Sprintf("%s-%d", f.Name, ordinal)
			if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage == "" {
				for _, session := range j.Inventory.Sessions {
					if session.Node == node {
						return fail(errors.New("observed runtime identity reactivation is unqualified: " + node))
					}
				}
			}
			for _, session := range j.Sessions {
				if session.Node == node && r.Options.LauncherImage == "" {
					return fail(errors.New("retained runtime identity reactivation is unqualified: " + node))
				}
			}
		}
		if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "" {
			if err := validateReactivation(j, op.From, op.To, f.Name); err != nil {
				return fail(err)
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
	if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "" && slices.ContainsFunc(j.PersistentHistory, func(m persistentMember) bool { return m.Retired && reactivatedNode(f.Name, m.Node, op.From, op.To) }) {
		op.Phase = "Reactivating"
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
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
