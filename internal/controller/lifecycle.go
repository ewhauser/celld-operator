package controller

import (
	"bytes"
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

const journalKey = "celld.eric.dev/lifecycle-journal"
const creationClaimsKey = "celld.eric.dev/creation-claim-uids"
const lossFenceKey = "celld.eric.dev/recovery-loss-fence"
const operationKey = "celld.eric.dev/lifecycle-operation"

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
	// Start of the current run of consecutive donor launcher failures during
	// Stopping. Zero whenever the last call succeeded. Optional and additive:
	// journals written before it simply decode as zero.
	DonorUnreachableSince time.Time
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

// journalRendering serializes the journal the way saveJournal will write it, or
// returns nil when it cannot be rendered.
func journalRendering(j *lifecycleJournal) []byte {
	b, err := json.Marshal(j)
	if err != nil {
		return nil
	}
	return b
}

// sameJournal reports whether the journal still renders exactly as the earlier
// rendering did, so a caller can tell a step that recorded something from one
// that recorded nothing. An unrenderable side compares as changed: this can only
// cause a write, never skip one.
func sameJournal(rendering []byte, j *lifecycleJournal) bool {
	current := journalRendering(j)
	return rendering != nil && current != nil && bytes.Equal(rendering, current)
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

// lifecycleOutcome is a terminal lifecycle result. A step returns nil instead
// when it established its invariant and the sequence must continue.
type lifecycleOutcome struct {
	result  ctrl.Result
	handled bool
	err     error
}

// lifecycleRun carries the per-reconcile lifecycle context shared by the steps
// below. The order of those steps, and the order of the journal writes inside
// them, is the crash-safety contract of ADR 0012 and must not be reordered.
type lifecycleRun struct {
	r   *Reconciler
	f   *fleet.CelldFleet
	res *fleet.CelldStorageReservation
	w   client.Object
	j   *lifecycleJournal
	op  *lifecycleOperation
}

// stop ends the lifecycle with a result an executor already produced.
func (s *lifecycleRun) stop(result ctrl.Result, handled bool, err error) *lifecycleOutcome {
	return &lifecycleOutcome{result: result, handled: handled, err: err}
}

// fail ends the lifecycle with a controller error and no status report.
func (s *lifecycleRun) fail(err error) *lifecycleOutcome {
	return &lifecycleOutcome{handled: true, err: err}
}

// block ends the lifecycle by reporting a blocking condition on the fleet.
func (s *lifecycleRun) block(ctx context.Context, reason, message string) *lifecycleOutcome {
	result, err := s.r.report(ctx, s.f, reason, message, false)
	return s.stop(result, true, err)
}

// progress ends the lifecycle by reporting forward progress on the fleet.
func (s *lifecycleRun) progress(ctx context.Context, message string) *lifecycleOutcome {
	return s.block(ctx, "LifecycleProgress", message)
}

// persist makes the journal durable and yields a terminal outcome only on
// failure, so a step can keep going once the write has survived the API.
func (s *lifecycleRun) persist(ctx context.Context) *lifecycleOutcome {
	if err := s.r.saveJournal(ctx, s.res, s.j); err != nil {
		return s.fail(err)
	}
	return nil
}

// save records a journal transition durably and ends the reconcile: no external
// action may follow an intent inside the reconcile that recorded it.
func (s *lifecycleRun) save(ctx context.Context) *lifecycleOutcome {
	if outcome := s.persist(ctx); outcome != nil {
		return outcome
	}
	return s.progress(ctx, "Durable lifecycle transition recorded; inspect status.lifecycle for the operation and target")
}

func (r *Reconciler) lifecycle(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, w client.Object) (ctrl.Result, bool, error) {
	s := &lifecycleRun{r: r, f: f, res: res, w: w}
	for _, step := range []func(context.Context) *lifecycleOutcome{
		s.loadOrBootstrapJournal,
		s.verifyWorkloadIdentity,
		s.copyWorkloadLossFence,
		s.verifyRetainedClaims,
		s.runMaintenanceOperation,
		s.verifyJournaledInfrastructure,
		s.schedulePersistentMembership,
		s.scheduleOrderedBucketMembership,
		s.observeRuntimeEvidence,
		s.releaseStaleMaintenanceFence,
		s.holdUnderMaintenanceFence,
		s.admitOperation,
		s.backfillOperationDeadline,
		s.finishCancellation,
		s.cancelSupersededRemoval,
		s.markOperationStalled,
		s.executeBucketContraction,
		s.executePersistentContraction,
		s.reportBlockedRemoval,
		s.refuseStalledIssuance,
		s.evaluateCapacityInFlight,
		s.refreshWorkloadVersion,
		s.executeExpansion,
		s.requireQualifiedFencing,
		s.adoptIssuedRemoval,
		s.issueRemovalIntent,
		s.confirmRemovalRecovery,
	} {
		if outcome := step(ctx); outcome != nil {
			return outcome.result, outcome.handled, outcome.err
		}
	}
	return ctrl.Result{}, false, nil
}

// loadOrBootstrapJournal establishes that a loadable journal backed by a recorded
// creation attempt exists before anything else reads or writes lifecycle state.
func (s *lifecycleRun) loadOrBootstrapJournal(ctx context.Context) *lifecycleOutcome {
	j, err := s.r.loadJournal(ctx, s.res)
	if err != nil {
		return s.block(ctx, "JournalInvalid", err.Error())
	}
	s.j = j
	if s.res.Annotations[attemptAnnotation] == "" {
		return s.block(ctx, "LifecycleBlocked", "Missing creation journal; refusing workload adoption")
	}
	if j != nil {
		return nil
	}
	return s.bootstrapJournal(ctx)
}

// bootstrapJournal establishes a first journal for a workload provisioned before
// one existed, only after proving the workload is undrifted and every initial
// retained claim still carries the exact identity creation recorded.
func (s *lifecycleRun) bootstrapJournal(ctx context.Context) *lifecycleOutcome {
	f, res, w := s.f, s.res, s.w
	baseline := f.DeepCopy()
	baseline.Spec.Replicas = replicas(w)
	if !matches(workload(baseline, s.r.Options), w) || w.GetAnnotations()[operationKey] != "" {
		return s.block(ctx, "LifecycleBlocked", "Workload drift or lost lifecycle journal; manual investigation required")
	}
	initial := res.Spec.InitialReplicas
	if initial == 0 {
		initial = replicas(w)
	}
	j := &lifecycleJournal{Version: 8, RuntimeImage: runtimeImage(f), Initial: initial, Applied: replicas(w), WorkloadUID: w.GetUID(), Claims: map[string]types.UID{}}
	s.j = j
	// Creation records claim UIDs before workload creation. Verify those bindings;
	// missing or replaced claims can never be adopted.
	var created map[string]types.UID
	if f.Spec.Profile == "PersistentFleet" {
		if err := json.Unmarshal([]byte(res.Annotations[creationClaimsKey]), &created); err != nil {
			return s.block(ctx, "StorageIdentityConflict", "Missing creation PVC UID inventory; retained disks require manual review")
		}
	}
	for _, claim := range initialClaims(baseline, w) {
		got := &corev1.PersistentVolumeClaim{}
		if err := s.r.Get(ctx, client.ObjectKeyFromObject(claim), got); err != nil {
			return s.block(ctx, "StorageIdentityConflict", err.Error())
		}
		createdUID, recorded := created[got.Name]
		if !recorded || createdUID != got.UID || got.Labels[FleetLabel] != string(f.UID) || got.Annotations["celld.eric.dev/storage-reservation"] != res.Name || !got.DeletionTimestamp.IsZero() {
			return s.block(ctx, "StorageIdentityConflict", "Initial retained claim binding changed")
		}
		j.Claims[got.Name] = got.UID
	}
	return s.persist(ctx)
}

// verifyWorkloadIdentity establishes that the journal still describes this exact
// workload object; a replacement is never adopted.
func (s *lifecycleRun) verifyWorkloadIdentity(ctx context.Context) *lifecycleOutcome {
	if s.j.WorkloadUID != s.w.GetUID() {
		return s.block(ctx, "LifecycleBlocked", "Workload UID changed; refusing replacement adoption")
	}
	return nil
}

// copyWorkloadLossFence establishes that a loss fence found on the workload is
// durably journaled before anything else. The workload fence is authoritative
// even if a leader crashed before the reservation journal could record the loss.
// It also invalidates old replica CASes.
func (s *lifecycleRun) copyWorkloadLossFence(ctx context.Context) *lifecycleOutcome {
	if loss := s.w.GetAnnotations()[lossFenceKey]; loss != "" && s.j.Loss == "" {
		s.j.Loss = loss
		return s.save(ctx)
	}
	return nil
}

// verifyRetainedClaims establishes that every journaled PVC still has its exact
// recorded identity, ownership and deletion state before any lifecycle action.
func (s *lifecycleRun) verifyRetainedClaims(ctx context.Context) *lifecycleOutcome {
	for name, uid := range s.j.Claims {
		got := &corev1.PersistentVolumeClaim{}
		if err := s.r.Get(ctx, types.NamespacedName{Namespace: s.f.Namespace, Name: name}, got); err != nil {
			return s.block(ctx, "StorageIdentityConflict", err.Error())
		}
		if got.UID != uid || !got.DeletionTimestamp.IsZero() || len(got.OwnerReferences) != 0 || got.Labels[FleetLabel] != string(s.f.UID) || got.Annotations["celld.eric.dev/storage-reservation"] != s.res.Name {
			return s.block(ctx, "StorageIdentityConflict", "Retained PVC identity, ownership or deletion state changed: "+name)
		}
	}
	return nil
}

// runMaintenanceOperation hands a recorded maintenance operation to its executor,
// which owns the rest of the reconcile while that operation is in flight.
func (s *lifecycleRun) runMaintenanceOperation(ctx context.Context) *lifecycleOutcome {
	if s.j.Maintenance == nil {
		return nil
	}
	return s.stop(s.r.executeMaintenance(ctx, s.f, s.res, s.j, s.w))
}

// verifyJournaledInfrastructure adopts the recorded operation and establishes
// that the live workload still equals the journaled infrastructure at the count
// that operation authorizes; drift is never repaired here.
func (s *lifecycleRun) verifyJournaledInfrastructure(ctx context.Context) *lifecycleOutcome {
	s.op = s.j.Operation
	expectedCount := s.j.Applied
	if s.op != nil && s.w.GetAnnotations()[operationKey] == s.op.ID {
		expectedCount = s.op.To
	}
	expected := s.f.DeepCopy()
	expected.Spec.Replicas = expectedCount
	expected.Spec.RuntimeImage = s.j.RuntimeImage
	if !matches(workload(expected, s.r.Options), s.w) {
		return s.block(ctx, "LifecycleBlocked", "Workload differs from journaled infrastructure; no template mutation or replica drift repair is allowed")
	}
	return nil
}

// schedulePersistentMembership establishes durable PersistentFleet membership
// scheduling, fencing a loss declaration onto the workload if one is found.
func (s *lifecycleRun) schedulePersistentMembership(ctx context.Context) *lifecycleOutcome {
	if s.f.Spec.Profile != "PersistentFleet" || s.r.Options.LauncherImage == "" {
		return nil
	}
	if err := s.r.schedulePersistent(ctx, appliedRuntime(s.f, s.j), s.res, s.j); err != nil {
		if _, loss := errors.AsType[*v050.LossError](err); loss {
			return s.stop(s.r.recordLoss(ctx, s.f, s.w, s.res, s.j, err.Error()))
		}
		return s.block(ctx, "PersistentSchedulingBlocked", err.Error())
	}
	return nil
}

// scheduleOrderedBucketMembership establishes ordered-Bucket session scheduling.
func (s *lifecycleRun) scheduleOrderedBucketMembership(ctx context.Context) *lifecycleOutcome {
	if !orderedBucket(s.f) {
		return nil
	}
	if err := s.r.scheduleOrderedBucket(ctx, s.f, s.j); err != nil {
		return s.block(ctx, "BucketSchedulingBlocked", err.Error())
	}
	return nil
}

// evidenceHeartbeat bounds how long the durable observation may lag the
// in-memory one while nothing else changes, so the reservation journal and the
// status.lifecycle.evidenceCheckedAt projection stay honest about the age of the
// last observation even when no decision followed it.
const evidenceHeartbeat = time.Minute

// observeRuntimeEvidence establishes a durably journaled runtime observation, and
// fences any possible-loss declaration before any further lifecycle action.
func (s *lifecycleRun) observeRuntimeEvidence(ctx context.Context) *lifecycleOutcome {
	if s.r.Evidence == nil {
		return nil
	}
	before := *s.j
	inventory, loss := s.r.Evidence.Observe(ctx, appliedRuntime(s.f, s.j), s.j.Inventory)
	s.j.Inventory = inventory
	if loss != "" && s.j.Loss == "" {
		return s.stop(s.r.recordLoss(ctx, s.f, s.w, s.res, s.j, "possible loss declaration: "+loss))
	}
	if s.steadyObservation(&before) {
		return nil
	}
	return s.persist(ctx)
}

// steadyObservation reports whether this observation may stay in memory until
// something acts on it. Every lifecycle transition writes the whole journal,
// including the inventory, and the five-second freshness bounds that gate an
// action compare the in-memory inventory against the clock inside the same
// reconcile. So a write is only skipped when the observation moved nothing but
// its own clocks, nothing is in flight that a crash would have to resume, and
// the durable observation is still younger than a heartbeat. After a crash the
// loaded journal carries an older CheckedAt, which fails those freshness bounds
// and forces a fresh observation before any action: the ADR 0012 order.
func (s *lifecycleRun) steadyObservation(before *lifecycleJournal) bool {
	j := s.j
	if j.Operation != nil || j.Maintenance != nil || j.Request != nil || j.BucketMigration != nil {
		return false
	}
	if !s.f.DeletionTimestamp.IsZero() {
		return false
	}
	if s.r.capacityNow().Sub(before.Inventory.CheckedAt) >= evidenceHeartbeat {
		return false
	}
	return sameJournal(journalRendering(withoutObservationClocks(before)), withoutObservationClocks(j))
}

// withoutObservationClocks copies a journal with the timestamps a steady-state
// observation always advances zeroed: the per-session FirstSeen/LastSeen stamps
// and the inventory CheckedAt. Everything else an observation can record -- a
// new session, a Current flag, an association, a blocker, a loss -- still
// compares, so a new field cannot silently join the ignored set.
func withoutObservationClocks(j *lifecycleJournal) *lifecycleJournal {
	out := *j
	out.Inventory.CheckedAt = time.Time{}
	out.Inventory.Sessions = slices.Clone(j.Inventory.Sessions)
	for i := range out.Inventory.Sessions {
		out.Inventory.Sessions[i].FirstSeen = time.Time{}
		out.Inventory.Sessions[i].LastSeen = time.Time{}
	}
	return &out
}

// releaseStaleMaintenanceFence establishes that a workload fence left behind by a
// crash is released only after maintenance capacity history is durably reset.
func (s *lifecycleRun) releaseStaleMaintenanceFence(ctx context.Context) *lifecycleOutcome {
	if maintenanceFence(s.f) != "" || s.w.GetAnnotations()[maintenanceFenceKey] == "" {
		return nil
	}
	// A crash may have left only the workload fence. Reset history durably
	// before releasing it, including when pause ended during that crash.
	resetMaintenanceCapacity(s.j)
	if outcome := s.persist(ctx); outcome != nil {
		return outcome
	}
	if err := s.r.setMaintenanceFence(ctx, s.f, s.w, ""); err != nil {
		return s.fail(err)
	}
	return s.progress(ctx, "Maintenance fence released; revalidate the recorded operation on the next reconcile")
}

// holdUnderMaintenanceFence establishes that a fenced fleet admits no new action:
// an unissued operation freezes with its settling observation cleared, while an
// already issued addition is still allowed to complete.
func (s *lifecycleRun) holdUnderMaintenanceFence(ctx context.Context) *lifecycleOutcome {
	if maintenanceFence(s.f) == "" {
		return nil
	}
	f, j, op := s.f, s.j, s.op
	if !f.DeletionTimestamp.IsZero() {
		if _, _, err := s.r.persistDisruptionRequest(ctx, f, s.res, j, s.w); err != nil {
			return s.fail(err)
		}
	}
	resetMaintenanceCapacity(j)
	issued := op != nil && s.w.GetAnnotations()[operationKey] == op.ID && replicas(s.w) == op.To
	if !issued {
		if op == nil && !f.DeletionTimestamp.IsZero() {
			return s.stop(s.r.disruption(ctx, f, s.res, j, s.w))
		}
		if op != nil {
			op.SettledAt = time.Time{}
		}
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
		return s.stop(ctrl.Result{}, false, nil)
	}
	if op.Phase == "Reactivating" || (f.Spec.Profile == "PersistentFleet" && op.To > op.From && slices.ContainsFunc(j.PersistentHistory, func(m persistentMember) bool {
		return m.Retired && reactivatedNode(f.Name, m.Node, op.From, op.To)
	})) {
		// Replica issuance can survive a crash before Reactivating is saved.
		// Preserve that authority too; pause cannot certify volume reuse.
		return s.stop(ctrl.Result{}, false, nil)
	}
	if op.To > op.From {
		j.History = append(j.History, completion(op, time.Time{}))
		j.Applied, j.Operation = op.To, nil
		return s.save(ctx)
	}
	return nil
}

// admitOperation establishes exactly one durable intent when no operation is
// recorded, after observational admission, disruption requests and the capacity
// gates. Recording an intent always ends the reconcile.
func (s *lifecycleRun) admitOperation(ctx context.Context) *lifecycleOutcome {
	if s.op != nil {
		return nil
	}
	if outcome := s.admitBucketObservations(ctx); outcome != nil {
		return outcome
	}
	if result, handled, err := s.r.disruption(ctx, s.f, s.res, s.j, s.w); handled || err != nil {
		return s.stop(result, handled, err)
	}
	recorded := journalRendering(s.j)
	target, automatic := s.r.capacityTarget(ctx, s.f, s.j)
	// Persist diagnostics even when a qualification gate will reject the request.
	// No replica action occurs until the later atomic journal+intent write succeeds.
	// An evaluation carries fresh sample stamps and observation times, so this
	// normally does write; the rendering comparison only skips a repeated
	// evaluation that recorded literally the same diagnostics, such as a policy
	// that has been disabled or handed to an external /scale writer.
	if s.j.Capacity != nil && !sameJournal(recorded, s.j) {
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
	}
	if target == s.j.Applied {
		return s.stop(ctrl.Result{}, false, nil)
	}
	if automatic {
		if outcome := s.revalidateCollectedRequest(ctx); outcome != nil {
			return outcome
		}
	}
	s.op = &lifecycleOperation{ID: string(uuid.NewUUID()), Phase: "Intent", StartedAt: s.r.capacityNow(), Deadline: s.r.capacityNow().Add(operationBudget), From: s.j.Applied, To: target, WorkloadVersion: s.w.GetResourceVersion(), Automatic: automatic}
	if outcome := s.recordBlockedRemovalIntent(ctx, automatic); outcome != nil {
		return outcome
	}
	if outcome := s.captureRemovalTarget(ctx); outcome != nil {
		return outcome
	}
	if automatic {
		s.op.PolicyHash = s.j.Capacity.Config
		s.op.ManualBaseline = s.f.Spec.Replicas
	}
	s.j.Operation = s.op
	if s.j.Capacity != nil {
		capacity.RecordAction(s.j.Capacity, s.r.capacityNow(), s.op.From, s.op.To)
	}
	return s.save(ctx) // No external action before the intent has survived an API write.
}

// admitBucketObservations establishes durably admitted observational Bucket
// history. Incomplete or uncertain admission must not prevent previously
// supported additive capacity: removal still revalidates all history
// independently and cannot use missing admission.
func (s *lifecycleRun) admitBucketObservations(ctx context.Context) *lifecycleOutcome {
	if s.f.Spec.Profile != "Bucket" || s.r.Evidence == nil {
		return nil
	}
	changed, admissionErr := s.r.admitBucketHistory(ctx, s.f, s.j)
	if _, loss := errors.AsType[*v050.LossError](admissionErr); loss {
		return s.stop(s.r.recordLoss(ctx, s.f, s.w, s.res, s.j, admissionErr.Error()))
	}
	if changed {
		return s.persist(ctx)
	}
	return nil
}

// revalidateCollectedRequest establishes that the fleet did not change while the
// capacity collector ran, so a recommendation never lands on a new request.
func (s *lifecycleRun) revalidateCollectedRequest(ctx context.Context) *lifecycleOutcome {
	latest := &fleet.CelldFleet{}
	if err := s.r.Get(ctx, client.ObjectKeyFromObject(s.f), latest); err != nil {
		return s.fail(err)
	}
	latest.Default()
	if latest.UID != s.f.UID || !latest.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(latest.Spec, s.f.Spec) {
		return s.block(ctx, "CapacityChanged", "Fleet changed during collection; discard the recommendation and reconcile the new request")
	}
	return nil
}

// recordBlockedRemovalIntent establishes durable blocked authority for a removal
// no qualified path may execute, without pretending its target or session
// evidence is qualified. Additive operations pass straight through.
func (s *lifecycleRun) recordBlockedRemovalIntent(ctx context.Context, automatic bool) *lifecycleOutcome {
	f, j, op := s.f, s.j, s.op
	if op.To >= op.From {
		return nil
	}
	if (f.Spec.Profile == "Bucket" || (f.Spec.Profile == "PersistentFleet" && s.r.Options.LauncherImage != "")) && s.r.Evidence != nil {
		op.To = op.From - 1
		op.Phase = "Blocked"
		if automatic {
			op.PolicyHash = j.Capacity.Config
			op.ManualBaseline = f.Spec.Replicas
		}
		j.Operation = op
		return s.save(ctx)
	}
	if s.r.Options.LocalTest && s.r.localLifecycle != nil {
		return nil
	}
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
	if outcome := s.persist(ctx); outcome != nil {
		return outcome
	}
	return s.reportRemovalBlock(ctx)
}

// reportRemovalBlock reports why production removal is unavailable, fencing a
// possible-loss finding onto the workload before the journal records it.
func (s *lifecycleRun) reportRemovalBlock(ctx context.Context) *lifecycleOutcome {
	reason, message := s.r.productionRemovalBlock(ctx, s.f, s.j)
	if reason == "PossibleDataLoss" && s.j.Loss == "" {
		return s.stop(s.r.recordLoss(ctx, s.f, s.w, s.res, s.j, message))
	}
	return s.block(ctx, reason, message)
}

// captureRemovalTarget establishes a complete, exact target and session inventory
// for a contraction the in-package qualification seam may execute: the highest
// ordinal, its pod UID and runtime generation, a live exact session, and every
// preserved historical session. Additive operations pass straight through.
func (s *lifecycleRun) captureRemovalTarget(ctx context.Context) *lifecycleOutcome {
	f, j, op := s.f, s.j, s.op
	if op.To >= op.From {
		return nil
	}
	if f.Spec.Profile == "Bucket" {
		return s.block(ctx, "BucketCompletionUnqualified", "Bucket completion authority is unqualified; every possible Deployment victim must pass preflight")
	}
	if !s.r.Options.LocalTest || s.r.localLifecycle == nil {
		return s.block(ctx, "FencingUnqualified", "Real process fencing and AWS recovery are unqualified; PersistentFleet contraction is unavailable")
	}
	if j.Loss != "" {
		return s.block(ctx, "PossibleDataLoss", j.Loss)
	}
	captured, err := s.r.localLifecycle.Capture(ctx, f, op.From-1, j.Sessions)
	if err != nil {
		return s.block(ctx, "RecoveryBlocked", err.Error())
	}
	if captured == nil {
		return s.block(ctx, "RecoveryBlocked", "No target/session capture")
	}
	captured.ID, captured.Phase, captured.From, captured.To, captured.WorkloadVersion = op.ID, "Intent", op.From, op.From-1, op.WorkloadVersion
	captured.Automatic = op.Automatic
	captured.StartedAt, captured.Deadline = op.StartedAt, op.Deadline
	s.op, op = captured, captured
	for _, previous := range j.Sessions {
		if !slices.Contains(op.Sessions, previous) {
			return s.block(ctx, "RecoveryBlocked", "Historical session was omitted or changed")
		}
	}
	if op.TargetPod != fmt.Sprintf("%s-%d", f.Name, op.To) || op.TargetUID == "" || op.TargetGeneration == "" || len(op.Sessions) == 0 {
		return s.block(ctx, "RecoveryBlocked", "Incomplete selected target/session inventory")
	}
	selected := false
	for _, session := range op.Sessions {
		if session.Node == op.TargetPod && session.Generation == op.TargetGeneration && session.Epoch > 0 && !session.Stopped {
			selected = true
		}
	}
	if !selected {
		return s.block(ctx, "RecoveryBlocked", "Selected target lacks a live exact session")
	}
	return nil
}

// backfillOperationDeadline establishes one durable deadline for a legacy
// in-flight record, on its first resumption.
func (s *lifecycleRun) backfillOperationDeadline(ctx context.Context) *lifecycleOutcome {
	if !s.op.StartedAt.IsZero() && !s.op.Deadline.IsZero() {
		return nil
	}
	s.op.StartedAt = s.r.capacityNow()
	s.op.Deadline = s.op.StartedAt.Add(operationBudget)
	return s.save(ctx)
}

// finishCancellation hands a canceling operation to the workload-CAS withdrawal.
func (s *lifecycleRun) finishCancellation(ctx context.Context) *lifecycleOutcome {
	if s.op.Phase != "Canceling" {
		return nil
	}
	result, err := s.r.cancelRemoval(ctx, s.res, s.j, s.w)
	return s.stop(result, true, err)
}

// cancelSupersededRemoval establishes that an unissued removal nobody wants any
// more enters cancellation. An addition supersedes any unissued removal. A manual
// removal is also withdrawn when spec.replicas returns to at least the original
// count: nobody wants it anymore. A recorded automatic removal is not withdrawn
// merely because the policy's current target equals the applied count (cooldown,
// stabilization); it freezes and rechecks before issuance. Every cancellation
// goes through the workload-CAS fence, so a delayed issuer cannot still win.
func (s *lifecycleRun) cancelSupersededRemoval(ctx context.Context) *lifecycleOutcome {
	op := s.op
	if op.To >= op.From || (op.Phase != "Intent" && op.Phase != "Blocked") || maintenanceFence(s.f) != "" {
		return nil
	}
	target, _ := s.r.capacityTarget(ctx, s.f, s.j)
	if target > op.From || (!op.Automatic && s.f.Spec.Replicas >= op.From) {
		op.Phase = "Canceling"
		return s.save(ctx)
	}
	return nil
}

// markOperationStalled records an expired deadline durably, exactly once.
func (s *lifecycleRun) markOperationStalled(ctx context.Context) *lifecycleOutcome {
	if s.r.capacityNow().Before(s.op.Deadline) || s.op.Stalled {
		return nil
	}
	s.op.Stalled = true
	return s.save(ctx)
}

// executeBucketContraction hands a recorded Bucket removal to its executor. The
// stabilization history cancelSupersededRemoval just refreshed is persisted
// first, including negative observations: a restart must not revive the earlier
// qualified low-demand window.
func (s *lifecycleRun) executeBucketContraction(ctx context.Context) *lifecycleOutcome {
	if s.f.Spec.Profile != "Bucket" || s.op.To >= s.op.From || s.r.Evidence == nil {
		return nil
	}
	if s.j.Capacity != nil {
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
	}
	return s.stop(s.r.contractBucket(ctx, s.f, s.res, s.j, s.w))
}

// executePersistentContraction hands a recorded PersistentFleet removal to its
// executor, persisting negative stabilization observations even when preflight
// fails, and routing an authorized manual 2-to-1 through coordinated downtime.
func (s *lifecycleRun) executePersistentContraction(ctx context.Context) *lifecycleOutcome {
	if s.f.Spec.Profile != "PersistentFleet" || s.op.To >= s.op.From || s.r.Options.LauncherImage == "" || s.r.Evidence == nil {
		return nil
	}
	if s.j.Capacity != nil {
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
	}
	if s.coordinatedDowntimeRemoval() {
		return s.stop(s.r.beginCoordinatedContraction(ctx, s.f, s.res, s.j, s.w))
	}
	return s.stop(s.r.contractPersistent(ctx, s.f, s.res, s.j, s.w))
}

// coordinatedDowntimeRemoval reports whether the recorded removal is the manual,
// still unissued 2-to-1 for which the fleet grants explicit downtime permission.
// The checks keep their original short-circuit order.
func (s *lifecycleRun) coordinatedDowntimeRemoval() bool {
	op := s.op
	if op.From != 2 || op.To != 1 || op.Automatic {
		return false
	}
	if op.Phase != "Blocked" && op.Phase != "Intent" {
		return false
	}
	return coordinatedDowntime(s.f)
}

// reportBlockedRemoval establishes that a blocked removal retains its authority
// and evidence without any effect.
func (s *lifecycleRun) reportBlockedRemoval(ctx context.Context) *lifecycleOutcome {
	if s.op.Phase != "Blocked" {
		return nil
	}
	if s.op.Stalled {
		return s.block(ctx, "OperationStalled", "Removal deadline exceeded; authority and evidence retained; compatible additions can supersede this unissued request")
	}
	return s.reportRemovalBlock(ctx)
}

// refuseStalledIssuance establishes that an expired operation has no further
// unissued effect. Expiration never certifies recovery or cancels an issued
// operation, so an already issued CAS is still reconstructed.
func (s *lifecycleRun) refuseStalledIssuance(ctx context.Context) *lifecycleOutcome {
	if s.op.Stalled && (s.w.GetAnnotations()[operationKey] != s.op.ID || replicas(s.w) != s.op.To) && s.op.Phase != "Recovering" {
		return s.block(ctx, "OperationStalled", "Operation deadline exceeded before replica issuance; authority retained")
	}
	return nil
}

// evaluateCapacityInFlight durably records the policy decision while an operation
// is in flight; the recorded target never changes. External mode has no policy to
// evaluate: its decision keeps naming the /scale writer as the owner.
func (s *lifecycleRun) evaluateCapacityInFlight(ctx context.Context) *lifecycleOutcome {
	f, j, op := s.f, s.j, s.op
	if maintenanceFence(f) != "" || f.Spec.Capacity == nil || j.Capacity == nil || externalOwner(f) {
		return nil
	}
	observation := capacity.Observation{At: s.r.capacityNow()}
	if s.r.Collector != nil {
		observation = s.r.Collector.Collect(ctx, f)
	}
	*j.Capacity = capacity.Evaluate(*f.Spec.Capacity, *j.Capacity, observation, max(op.From, op.To))
	if j.Capacity.Decision.Reason != "PendingCapacity" && j.Capacity.Decision.Reason != "IneffectiveCapacity" {
		j.Capacity.Decision.Reason = "LifecycleInFlight"
		j.Capacity.Decision.Message = "Recorded operation retains its original target; new policy and manual requests wait"
	}
	j.Capacity.Decision.DesiredReplicas = op.To
	return s.persist(ctx)
}

// refreshWorkloadVersion durably replaces an obsolete workload version before any
// effect. An obsolete version cannot still win a CAS; the operation revalidates
// on another reconcile before attempting anything.
func (s *lifecycleRun) refreshWorkloadVersion(ctx context.Context) *lifecycleOutcome {
	op := s.op
	if op.Phase != "Intent" && op.Phase != "Prepared" {
		return nil
	}
	if replicas(s.w) != op.From || s.w.GetResourceVersion() == op.WorkloadVersion {
		return nil
	}
	op.WorkloadVersion = s.w.GetResourceVersion()
	return s.save(ctx)
}

// executeExpansion hands an additive operation to the scale-out executor.
func (s *lifecycleRun) executeExpansion(ctx context.Context) *lifecycleOutcome {
	if s.op.To <= s.op.From {
		return nil
	}
	return s.stop(s.r.expand(ctx, s.f, s.res, s.j, s.w))
}

// requireQualifiedFencing establishes that only the in-package qualification seam
// executes a removal, and never after a possible-loss finding.
func (s *lifecycleRun) requireQualifiedFencing(ctx context.Context) *lifecycleOutcome {
	if !s.r.Options.LocalTest || s.r.localLifecycle == nil {
		return s.block(ctx, "FencingUnqualified", "Operation retained; qualified process fencing is unavailable")
	}
	if s.j.Loss != "" {
		return s.block(ctx, "PossibleDataLoss", s.j.Loss)
	}
	return nil
}

// adoptIssuedRemoval reconstructs a replica CAS that won before the journal could
// record it, moving the recorded intent into recovery.
func (s *lifecycleRun) adoptIssuedRemoval(ctx context.Context) *lifecycleOutcome {
	if s.op.Phase == "Intent" && replicas(s.w) == s.op.To && s.w.GetAnnotations()[operationKey] == s.op.ID {
		s.op.Phase = "Recovering"
		return s.save(ctx)
	}
	return nil
}

// issueRemovalIntent establishes revalidated membership and fresh preflight (and,
// for an automatic removal, low-demand) evidence, then issues the single replica
// CAS this operation is authorized to win.
func (s *lifecycleRun) issueRemovalIntent(ctx context.Context) *lifecycleOutcome {
	if s.op.Phase != "Intent" {
		return nil
	}
	f, j, op := s.f, s.j, s.op
	// Desired count changes never retarget an existing operation. Before issue,
	// freeze rather than cancel: a delayed previous leader could still issue it.
	if !op.Automatic && f.Spec.Replicas >= op.From {
		return s.block(ctx, "DesiredChanged", "Existing removal intent retained; no new removal authorized")
	}
	if err := s.r.localLifecycle.Validate(ctx, f, op); err != nil {
		return s.block(ctx, "RecoveryBlocked", err.Error())
	}
	adapter, err := catalog.New(j.RuntimeImage)
	if err != nil {
		return s.fail(err)
	}
	evidence, err := adapter.Inspect(ctx, s.r.localLifecycle.Reader(f), v050.Request{OperationID: op.ID, Sessions: op.Sessions, InventoryComplete: true, CapturedAt: s.r.localLifecycle.Now(), MaxAge: 5 * time.Second, PageBudget: 1000}, s.r.localLifecycle.Now)
	if err != nil {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return s.stop(s.r.recordLoss(ctx, f, s.w, s.res, j, err.Error()))
		}
		return s.block(ctx, "RecoveryBlocked", err.Error())
	}
	if err := s.r.localLifecycle.Validate(ctx, f, op); err != nil {
		return s.block(ctx, "RecoveryBlocked", err.Error())
	}
	if !evidenceFresh(evidence, s.r.localLifecycle.Now()) {
		return s.block(ctx, "RecoveryBlocked", "Preflight evidence expired during membership revalidation")
	}
	if op.Automatic {
		if outcome := s.revalidateAutomaticRemoval(ctx, evidence); outcome != nil {
			return outcome
		}
	}
	if err := s.r.applyReplicas(ctx, s.w, op); err != nil {
		return s.block(ctx, "ReplicaUpdateBlocked", err.Error())
	}
	op.Phase = "Recovering"
	return s.save(ctx)
}

// revalidateAutomaticRemoval establishes that an automatic removal is still the
// one the policy recorded, and that complete fresh low-demand evidence supports
// it, immediately before issuance.
func (s *lifecycleRun) revalidateAutomaticRemoval(ctx context.Context, evidence v050.Evidence) *lifecycleOutcome {
	if !s.automaticIntentAuthorized() {
		return s.block(ctx, "CapacityChanged", "Automatic removal intent retained; changed policy/manual request requires review")
	}
	observation := s.r.Collector.Collect(ctx, s.f)
	if !s.lowDemandEstablished(observation) {
		return s.block(ctx, "CapacityUncertain", "Fresh complete low-demand evidence is required before automatic removal")
	}
	if !evidenceFresh(evidence, s.r.localLifecycle.Now()) {
		return s.block(ctx, "RecoveryBlocked", "Recovery evidence expired during capacity revalidation")
	}
	return nil
}

// automaticIntentAuthorized reports whether the live policy, manual baseline and
// collector still match the recorded automatic intent. The checks keep their
// original short-circuit order: the policy presence check guards the rest.
func (s *lifecycleRun) automaticIntentAuthorized() bool {
	f, op := s.f, s.op
	if f.Spec.Capacity == nil || f.Spec.Capacity.Mode != "Automatic" {
		return false
	}
	if op.PolicyHash != s.j.Capacity.Config || op.ManualBaseline != f.Spec.Replicas {
		return false
	}
	return op.To >= f.Spec.Capacity.MinReplicas && s.r.Collector != nil
}

// lowDemandEstablished reports whether a complete, stabilized and fresh low-demand
// window supports removing one replica from the operation's original count.
func (s *lifecycleRun) lowDemandEstablished(observation capacity.Observation) bool {
	policy, state := *s.f.Spec.Capacity, s.j.Capacity
	if state.LowSince.IsZero() || state.LowSamples < policy.MinSamples {
		return false
	}
	if observation.At.Sub(state.LowSince) < capacity.Seconds(policy.ScaleInStabilizationSeconds) {
		return false
	}
	if !capacity.LowDemand(policy, observation, s.op.From) {
		return false
	}
	return !observation.At.After(s.r.capacityNow()) && s.r.capacityNow().Sub(observation.At) <= capacity.Seconds(policy.MaxAgeSeconds)
}

// recoveryBlock reports a blocking recovery condition after durably clearing any
// settling observation: uncertainty restarts settling, never completes it.
func (s *lifecycleRun) recoveryBlock(ctx context.Context, reason, message string) *lifecycleOutcome {
	if !s.op.SettledAt.IsZero() {
		s.op.SettledAt = time.Time{}
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
	}
	return s.block(ctx, reason, message)
}

// confirmRemovalRecovery establishes an exact process fence plus two complete,
// revalidated assessments at least the settling interval apart before the removal
// is recorded as complete with its full session history retained.
func (s *lifecycleRun) confirmRemovalRecovery(ctx context.Context) *lifecycleOutcome {
	f, j, op := s.f, s.j, s.op
	if err := s.r.localLifecycle.Validate(ctx, f, op); err != nil {
		return s.recoveryBlock(ctx, "RecoveryBlocked", err.Error())
	}
	stopped, err := s.r.localLifecycle.Stopped(ctx, f, op)
	if err != nil || !stopped {
		return s.recoveryBlock(ctx, "FencingRequired", "Exact selected process termination is unconfirmed; retain disk and investigate fencing")
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
		return s.block(ctx, "RecoveryBlocked", "Selected runtime session is absent from durable inventory")
	}
	adapter, err := catalog.New(j.RuntimeImage)
	if err != nil {
		return s.fail(err)
	}
	evidence, err := adapter.Assess(ctx, s.r.localLifecycle.Reader(f), v050.Request{OperationID: op.ID, Sessions: sessions, InventoryComplete: true, CapturedAt: s.r.localLifecycle.Now(), MaxAge: 5 * time.Second, PageBudget: 1000}, s.r.localLifecycle.Now)
	if err != nil {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return s.stop(s.r.recordLoss(ctx, f, s.w, s.res, j, err.Error()))
		}
		op.SettledAt = time.Time{} // uncertainty restarts settling, never completes it
		if outcome := s.persist(ctx); outcome != nil {
			return outcome
		}
		return s.block(ctx, "RecoveryBlocked", err.Error())
	}
	if err := s.r.localLifecycle.Validate(ctx, f, op); err != nil {
		return s.recoveryBlock(ctx, "RecoveryBlocked", err.Error())
	}
	if !evidenceFresh(evidence, s.r.localLifecycle.Now()) {
		return s.recoveryBlock(ctx, "RecoveryBlocked", "Recovery evidence expired during membership revalidation")
	}
	if op.SettledAt.IsZero() {
		op.SettledAt = evidence.ObservedAt
		return s.save(ctx)
	}
	if evidence.ObservedAt.Sub(op.SettledAt) < 10*time.Second {
		return s.progress(ctx, "Recovery evidence revalidated; waiting for another full assessment after settling")
	}
	j.History = append(j.History, completion(op, evidence.ObservedAt))
	j.Sessions, j.Applied, j.Operation = sessions, op.To, nil
	return s.save(ctx)
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
	// Durable intent exists; the effect has not been issued.
	r.faultPoint("before-effect")
	if err := r.Update(ctx, w); err != nil {
		return err
	}
	// The effect is durable on the workload; the journal has not recorded it.
	r.faultPoint("after-effect")
	return nil
}

func (r *Reconciler) expand(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	fail := func(err error) (ctrl.Result, bool, error) {
		result, reportErr := r.report(ctx, f, "ScaleOutBlocked", err.Error(), false)
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
	result, err := r.report(ctx, f, "PossibleDataLoss", message, false)
	return result, true, err
}
