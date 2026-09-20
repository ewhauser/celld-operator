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

// transition is the (result, handled, error) triple every lifecycle phase
// returns. Carrying it as a value lets the shared helpers below hand a caller's
// own terminal transition back to it unchanged, instead of inventing one.
type transition struct {
	result  ctrl.Result
	handled bool
	err     error
}

func asTransition(result ctrl.Result, handled bool, err error) *transition {
	return &transition{result: result, handled: handled, err: err}
}

func (t *transition) unwrap() (ctrl.Result, bool, error) { return t.result, t.handled, t.err }

// settlingWindow is the convergence window every recovery phase waits out
// before it may record history and advance.
const settlingWindow = 10 * time.Second

// awaitSettling opens and waits out the settling window. The first observation
// is recorded through settledAt and durably persisted with save; a later
// observation still inside the window returns wait. A nil return means the
// window has elapsed and the caller may proceed. Clearing settledAt on the way
// out stays with the caller: phases that step to a further target reset it,
// phases that finish the operation do not.
func awaitSettling(settledAt *time.Time, observedAt time.Time, save, wait func() (ctrl.Result, bool, error)) *transition {
	if settledAt.IsZero() {
		*settledAt = observedAt
		return asTransition(save())
	}
	if observedAt.Sub(*settledAt) < settlingWindow {
		return asTransition(wait())
	}
	return nil
}

// requeueSoon is the wait every maintenance phase uses inside the settling
// window: no progress is recorded, the pass is simply repeated.
func requeueSoon() (ctrl.Result, bool, error) {
	return ctrl.Result{RequeueAfter: time.Second}, true, nil
}

// scaleToZero is the replica CAS that removes all compute for an operation and
// stamps the workload with the exact operation that issued it. admit runs only
// while the workload still carries replicas and states the phase's own
// preconditions; the transition it returns denies the CAS and is handed back
// unchanged. A workload already at zero must carry this operation's annotation,
// or unauthorized is returned: a foreign or absent stamp means some other
// issuer removed the replicas and this operation has no authority over them.
// A nil return means the caller may proceed to its next phase.
func (r *Reconciler) scaleToZero(ctx context.Context, w client.Object, id string, admit, unauthorized func() *transition) *transition {
	if replicas(w) != 0 {
		if denied := admit(); denied != nil {
			return denied
		}
		setReplicas(w, 0)
		w.GetAnnotations()[operationKey] = id
		if err := r.Update(ctx, w); err != nil {
			return asTransition(ctrl.Result{}, true, err)
		}
		return nil
	}
	if w.GetAnnotations()[operationKey] != id {
		return unauthorized()
	}
	return nil
}

func (r *Reconciler) beginMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	if w.GetUID() != j.WorkloadUID {
		return ctrl.Result{}, true, errors.New("maintenance workload changed")
	}
	if j.Loss != "" {
		result, err := r.report(ctx, f, "PossibleDataLoss", j.Loss, false)
		return result, true, err
	}
	if j.Operation != nil {
		return ctrl.Result{}, false, nil
	}
	j.Maintenance = &maintenanceOperation{ID: j.Request.ID, Kind: j.Request.Kind, Token: j.Request.RestartToken, Phase: "Capture", StartedAt: r.capacityNow(), Deadline: r.capacityNow().Add(operationBudget)}
	return r.saveMaintenance(ctx, f, res, j)
}

// maintenancePass is one reconcile pass over an admitted maintenance operation.
// It carries what every phase of that pass needs: the fleet, reservation,
// journal and workload, the operation itself, the journal view each assessment
// is made against, and the two closures a phase ends in — save, which durably
// records the transition, and block, which records a blocker and reports it.
type maintenancePass struct {
	f     *fleet.CelldFleet
	res   *fleet.CelldStorageReservation
	j     *lifecycleJournal
	w     client.Object
	m     *maintenanceOperation
	view  lifecycleJournal
	save  func() (ctrl.Result, bool, error)
	block func(error) (ctrl.Result, bool, error)
}

func (r *Reconciler) executeMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	m := j.Maintenance
	p := &maintenancePass{f: f, res: res, j: j, w: w, m: m}
	p.save = func() (ctrl.Result, bool, error) {
		return r.saveMaintenance(ctx, f, res, j)
	}
	p.block = func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		if e, ok := errors.AsType[*v050.BucketExpiryInvalidatedError](err); ok {
			invalidateBucketExpiry([][]bucketSession{m.Sessions, j.BucketHistory}, e)
		}
		m.SettledAt = time.Time{}
		if saveErr := r.saveJournal(ctx, res, j); saveErr != nil {
			return ctrl.Result{}, true, saveErr
		}
		result, reportErr := r.report(ctx, f, "MaintenanceRecoveryBlocked", err.Error(), false)
		return result, true, reportErr
	}
	expected, denied := r.admitMaintenancePass(ctx, p)
	if denied != nil {
		return denied.unwrap()
	}
	if f.Spec.Profile == "PersistentFleet" {
		beforeVersion := expected.ResourceVersion
		result, handled, err := r.executePersistentMaintenance(ctx, expected, res, j, w, p.block)
		if expected.ResourceVersion != beforeVersion {
			f.ResourceVersion, f.Status = expected.ResourceVersion, expected.Status
		}
		return result, handled, err
	}
	p.view = *j
	p.view.Operation = &lifecycleOperation{ID: m.ID, Phase: "Recovering", From: j.Applied, To: j.Applied, BucketCandidates: m.Sessions}
	if m.Kind == "Delete" {
		return r.executeBucketDeletion(ctx, f, res, j, w, p.block)
	}
	switch m.Phase {
	case "Capture", "Next":
		return r.executeMaintenanceCapture(ctx, p)
	case "Authorized":
		return r.executeMaintenanceAuthorized(ctx, p)
	case "Recovering":
		return r.executeMaintenanceRecovering(ctx, p)
	default:
		return p.block(errors.New("unknown maintenance phase"))
	}
}

// admitMaintenancePass re-establishes on every pass that this operation still
// holds authority over this exact workload: a valid journal, no loss fence, an
// unchanged workload identity and template, a current maintenance fence and
// available production evidence. It returns the expected fleet the pass
// observed against, or the transition the caller must return instead.
func (r *Reconciler) admitMaintenancePass(ctx context.Context, p *maintenancePass) (*fleet.CelldFleet, *transition) {
	f, j, w, m := p.f, p.j, p.w, p.m
	if invalidMaintenanceAuthority(j) {
		return nil, asTransition(p.block(errors.New("invalid maintenance authority")))
	}
	if j.Loss != "" || w.GetAnnotations()[lossFenceKey] != "" {
		return nil, asTransition(p.block(errors.New("durable loss fence prohibits maintenance completion")))
	}
	expected := appliedRuntime(f, j)
	if m.Kind == "Upgrade" {
		image, err := transitionWorkloadImage(j, w)
		if err != nil {
			return nil, asTransition(p.block(err))
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
		return nil, asTransition(p.block(errors.New("maintenance workload identity or configuration changed")))
	}
	if w.GetAnnotations()[maintenanceFenceKey] != maintenanceFence(f) {
		if err := r.setMaintenanceFence(ctx, f, w, maintenanceFence(f)); err != nil {
			return nil, asTransition(p.block(err))
		}
	}
	if orderedBucket(f) {
		if err := r.scheduleOrderedBucket(ctx, f, j); err != nil {
			return nil, asTransition(p.block(err))
		}
	}
	if r.Evidence == nil {
		return nil, asTransition(p.block(errors.New("production recovery evidence unavailable")))
	}
	inventory, loss := r.Evidence.Observe(ctx, expected, j.Inventory)
	j.Inventory = inventory
	if loss != "" {
		return nil, asTransition(r.recordLoss(ctx, f, w, p.res, j, loss))
	}
	if m.Phase == "Capture" || m.Phase == "Next" {
		if t := r.admitNextMaintenanceAction(ctx, p); t != nil {
			return nil, t
		}
	}
	return expected, nil
}

// admitNextMaintenanceAction gates the admission of the next disruptive action.
// Pause and a request that no longer asks for this operation stop the next
// action; neither revokes an action a previous leader already authorized.
func (r *Reconciler) admitNextMaintenanceAction(ctx context.Context, p *maintenancePass) *transition {
	f, j, m := p.f, p.j, p.m
	if paused(f) {
		result, err := r.report(ctx, f, "MaintenancePaused", "No new maintenance action admitted", false)
		return asTransition(result, true, err)
	}
	withdrawn := false
	switch m.Kind {
	case "Upgrade":
		withdrawn = !canStopUpgrade(f, j, r.Options) || runtimeImage(f) != m.TargetImage || !f.DeletionTimestamp.IsZero()
	case "Contract":
		withdrawn = f.Spec.Replicas != m.TargetReplicas || !f.DeletionTimestamp.IsZero() || !coordinatedDowntime(f)
	case "Restart":
		withdrawn = !f.DeletionTimestamp.IsZero() || runtimeImage(f) != j.RuntimeImage || f.Spec.Maintenance == nil || f.Spec.Maintenance.RestartToken != m.Token
	}
	if withdrawn {
		j.Maintenance = nil
		return asTransition(p.save())
	}
	if !r.capacityNow().Before(m.Deadline) {
		return asTransition(p.block(errors.New("maintenance admission deadline exceeded")))
	}
	return nil
}

// invalidMaintenanceAuthority reports whether the journal's maintenance record
// could never have been admitted: no operation identity, an unknown kind, a
// target index outside the captured inventory, or a concurrent scaling
// operation.
func invalidMaintenanceAuthority(j *lifecycleJournal) bool {
	m := j.Maintenance
	if j.Operation != nil || m.ID == "" {
		return true
	}
	if m.Kind != "Restart" && m.Kind != "Delete" && m.Kind != "Contract" && m.Kind != "Upgrade" {
		return true
	}
	return m.Index < 0 || m.Index > len(m.Targets)
}

func (r *Reconciler) executeMaintenanceCapture(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, j, w, m, save, block := p.f, p.j, p.w, p.m, p.save, p.block
	assessment, err := r.assessBucket(ctx, f, &p.view, j.Applied, false, bucketScopeCapacity)
	if err != nil {
		return block(err)
	}
	sessions := assessment.Sessions
	if m.Phase == "Capture" {
		pods, err := r.Evidence.pods(ctx, f)
		if err != nil {
			return block(err)
		}
		// Capture is replayed whenever a later step in this pass blocks, so the
		// inventory is rebuilt and replaced rather than appended to. Appending
		// duplicated every target and wedged the journal on its next load.
		targets := make([]maintenanceTarget, 0, len(pods))
		for _, pod := range pods {
			targets = append(targets, maintenanceTarget{Name: pod.Name, UID: pod.UID})
		}
		slices.SortFunc(targets, func(a, b maintenanceTarget) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		})
		m.Targets = targets
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
	// The assessment's closing sweep already re-read every candidate and refused
	// to return unless it matched the opening one, so this pass holds a fenced,
	// unchanged candidate set. Sweeping a third time here read the same objects
	// again; the identity fence around the S3 read still guarantees that what
	// placement is checked against is what the assessment admitted.
	if err := validateRestartPlacement(f, assessment.Candidates, target.UID); err != nil {
		return block(err)
	}
	if err := r.authorizeMaintenanceAction(ctx, w, m); err != nil {
		return block(err)
	}
	m.Phase = "Authorized"
	return save()
}

func (r *Reconciler) executeMaintenanceAuthorized(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, j, m, save, block := p.f, p.j, p.m, p.save, p.block
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
		assessment, err := r.assessBucket(ctx, f, &p.view, j.Applied, false, bucketScopeCapacity)
		if err != nil {
			return block(err)
		}
		if !slices.Equal(assessment.Sessions, m.Sessions) {
			m.Sessions = assessment.Sessions
			return save()
		}
		// Same fence as in Capture: the assessment closed with a candidate sweep
		// that had to equal its opening one, so placement is checked against the
		// exact set this assessment admitted.
		if err := validateRestartPlacement(f, assessment.Candidates, target.UID); err != nil {
			return block(err)
		}
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &target.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return block(err)
		}
	}
	m.Phase = "Recovering"
	return save()
}

func (r *Reconciler) executeMaintenanceRecovering(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, j, m, save, block := p.f, p.j, p.m, p.save, p.block
	sessions, at, err := r.bucketAssessment(ctx, f, &p.view, j.Applied, true)
	if err != nil {
		return block(err)
	}
	target := m.Targets[m.Index]
	if !slices.ContainsFunc(sessions, func(s bucketSession) bool { return s.Node == string(target.UID) && s.Retired && s.ExpiryObserved }) {
		return block(errors.New("restart target has not retired with positive lease expiry"))
	}
	m.Sessions = sessions
	if t := awaitSettling(&m.SettledAt, at, save, requeueSoon); t != nil {
		return t.unwrap()
	}
	j.BucketHistory = sessions
	m.Index++
	m.Phase = "Next"
	m.SettledAt = time.Time{}
	return save()
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
		// Shutdown has no projected survivor placement/capacity requirement, so it
		// demands neither low demand nor a Metrics Server collector. Current
		// membership, leases and no-log S3 obligations must nevertheless be fully
		// assessed.
		sessions, _, err := r.bucketAssessmentMode(ctx, f, &view, j.Applied, true, bucketScopeShutdown)
		if err != nil {
			return block(err)
		}
		m.Sessions = sessions
		m.Phase = "Authorized"
		return save()
	case "Authorized":
		admit := func() *transition {
			if replicas(w) != j.Applied || w.GetAnnotations()[maintenanceFenceKey] != "deleting" {
				return asTransition(block(errors.New("deletion workload fence unavailable")))
			}
			return nil
		}
		unauthorized := func() *transition {
			return asTransition(block(errors.New("zero replicas lacks deletion authority")))
		}
		if t := r.scaleToZero(ctx, w, m.ID, admit, unauthorized); t != nil {
			return t.unwrap()
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
			members = append(members, v050.BucketMember{Node: s.Node, Generation: s.Generation, SupersededBy: s.SupersededBy, Retired: true, Resolved: resolvedBucketSession(s)})
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
		if t := awaitSettling(&m.SettledAt, evidence.ObservedAt, save, requeueSoon); t != nil {
			return t.unwrap()
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
	if invalidMaintenanceAuthority(j) {
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
	_, err := r.report(ctx, f, "LifecycleProgress", "Durable maintenance transition recorded; inspect lifecycle maintenance phase", false)
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
