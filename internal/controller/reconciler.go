// Package controller provisions experimental fleet infrastructure with durable manual lifecycle intent.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/fencing"
	"github.com/ewhauser/celld-operator/internal/launcher"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const attemptAnnotation = "celld.eric.dev/workload-creation-attempted"

// Reconciler uses an uncached client for the durable reservation and creation journal.
// Kubernetes Create is the cross-controller arbitration point; leader election is not the safety proof.
type Reconciler struct {
	client.Client
	Options               Options
	Infrastructure        fencing.API
	NetworkPolicyEnforced bool
	Recorder              events.EventRecorder
	// localLifecycle is only supplied by in-package qualification tests. No production fence exists.
	localLifecycle lifecycleEvidence
	launcherCall   func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error)
	Evidence       *ProductionEvidence
	Collector      capacityCollector
	now            func() time.Time
}

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func specHash(f *fleet.CelldFleet) string {
	spec := f.Spec
	// Preserve reservation hashes from before the opt-in Ordered layout existed.
	if spec.BucketWorkload == "Deployment" {
		spec.BucketWorkload = ""
	}
	spec.Capacity = nil
	spec.RuntimeImage = ""
	spec.Maintenance = nil
	b, _ := json.Marshal(spec)
	return digest(b)
}
func reservationName(f *fleet.CelldFleet) string {
	return "s3-" + digest([]byte(f.Spec.Storage.Bucket))[:56]
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	f := &fleet.CelldFleet{}
	if err := r.Get(ctx, req.NamespacedName, f); err != nil {
		if apierrors.IsNotFound(err) {
			clearFleetMetrics(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	result, err := r.reconcileFleet(ctx, f)
	if apierrors.IsForbidden(err) {
		// The cluster role covers only the fleet API; every fleet namespace needs
		// the namespaced Role from config/rbac/fleet-namespace.yaml. Report that
		// instead of requeueing silently, and never touch namespace resources.
		f.Default()
		return r.report(ctx, f, nil, "NamespaceAccessDenied", "Operator lacks the fleet-namespace Role in "+f.Namespace+"; apply config/rbac/fleet-namespace.yaml there ("+err.Error()+")", false)
	}
	return result, err
}

func (r *Reconciler) reconcileFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	f.Default()
	if !f.DeletionTimestamp.IsZero() {
		return r.deleteFleet(ctx, f)
	}
	if err := f.Validate(); err != nil {
		return r.report(ctx, f, nil, "InvalidConfiguration", err.Error(), false)
	}
	if !controllerutil.ContainsFinalizer(f, Finalizer) {
		base := f.DeepCopy()
		controllerutil.AddFinalizer(f, Finalizer)
		if err := r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if paused(f) {
		return r.pauseFleet(ctx, f)
	}
	if !r.NetworkPolicyEnforced {
		return r.report(ctx, f, nil, "IsolationUnverified", "Administrator must verify a NetworkPolicy enforcing CNI and enable --network-policy-enforced before provisioning", false)
	}
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Spec.ServiceAccountName}, sa); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, nil, "ServiceAccountMissing", "Referenced ServiceAccount does not exist in the fleet namespace", false)
	}
	if f.Spec.Profile == "PersistentFleet" {
		sc := &storagev1.StorageClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: f.Spec.Storage.StorageClassName}, sc); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, nil, "StorageClassMissing", "Referenced StorageClass does not exist", false)
		}
		if sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain || sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || (!r.Options.LocalTest && sc.Provisioner != "ebs.csi.aws.com") {
			return r.report(ctx, f, nil, "InvalidStorageClass", "Requires EBS CSI, Retain reclaim policy and WaitForFirstConsumer binding (local test permits a different provisioner)", false)
		}
	}
	reservation := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{InitialReplicas: f.Spec.Replicas, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
	expected := reservation.Spec
	if err := r.Create(ctx, reservation); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: reservation.Name}, reservation); err != nil {
			return ctrl.Result{}, err
		}
	}
	// One hydration per reconcile (ADR 0021 phase 1). Bucket migration,
	// reservation matching, the lifecycle run and every report below read this
	// journal; none of them loads again.
	h := r.hydrate(ctx, reservation)
	if h.err != nil {
		// A journal that cannot be read is not a scope conflict. Report the read
		// failure itself, as the lifecycle's own load does.
		return r.report(ctx, f, h, "JournalInvalid", h.err.Error(), false)
	}
	if result, handled, err := r.migrateBucket(ctx, f, h); handled || err != nil {
		return result, err
	}
	if len(reservation.OwnerReferences) != 0 || !reservation.DeletionTimestamp.IsZero() || !r.reservationMatches(ctx, f, h, expected) {
		return r.report(ctx, f, h, "StorageScopeConflict", "Bucket is permanently reserved to another fleet UID or immutable configuration; no resources adopted", false)
	}

	for _, obj := range prerequisites(f, r.Options) {
		if err := r.ensure(ctx, obj); err != nil {
			return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
		}
	}
	desired := workload(f, r.Options)
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		if !knownRuntime(runtimeImage(f)) || (runtimeImage(f) != Image && (f.Spec.Profile != "PersistentFleet" || r.Options.LauncherImage == "")) {
			return r.report(ctx, f, h, "UnsupportedTransition", "Initial runtime requires a qualified release; v0.4.1 requires PersistentFleet with the trusted launcher", false)
		}
		if reservation.Annotations[attemptAnnotation] != "" {
			return r.report(ctx, f, h, "LifecycleBlocked", "Workload is missing after a recorded creation attempt; automatic recreation could reuse an unsafe identity or disk", false)
		}
		if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "" {
			if err := r.createLauncherKey(ctx, f, reservation); err != nil {
				return r.report(ctx, f, h, "LauncherIdentityBlocked", err.Error(), false)
			}
		}
		claims := initialClaims(f, desired)
		if err := r.checkInitialClaims(ctx, claims); err != nil {
			return r.report(ctx, f, h, "StorageIdentityConflict", err.Error(), false)
		}
		// Persist intent BEFORE Create. A crash in this window intentionally blocks for review.
		if reservation.Annotations == nil {
			reservation.Annotations = make(map[string]string)
		}
		reservation.Annotations[attemptAnnotation] = "true"
		if err := r.Update(ctx, reservation); err != nil {
			return ctrl.Result{}, err
		}
		createdClaims := map[string]types.UID{}
		for _, claim := range claims {
			if err := r.Create(ctx, claim); err != nil {
				return r.report(ctx, f, h, "StorageIdentityConflict", fmt.Sprintf("Cannot exclusively create PVC %s: %v; retained claims require manual review", claim.Name, err), false)
			}
			createdClaims[claim.Name] = claim.UID
		}
		claimIDs, err := json.Marshal(createdClaims)
		if err != nil {
			return ctrl.Result{}, err
		}
		reservation.Annotations[creationClaimsKey] = string(claimIDs)
		if err := r.Update(ctx, reservation); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "Provisioning", "Initial workload created; waiting for runtime readiness and placement", true)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if result, handled, err := r.lifecycle(ctx, f, h, actual); handled || err != nil {
		return result, err
	}
	// In automatic mode, the journal owns applied capacity, while spec.replicas
	// remains the user's manual command field. Never reconcile it back implicitly.
	if f.Spec.Capacity != nil {
		setReplicas(desired, replicas(actual))
	}
	if !matches(desired, actual) {
		return r.report(ctx, f, h, "LifecycleBlocked", "Existing workload differs from the journaled spec; no rollout, adoption or drift repair is authorized", false)
	}
	if reservation.Annotations[attemptAnnotation] == "" {
		return r.report(ctx, f, h, "LifecycleBlocked", "Existing workload has no creation journal; refusing adoption", false)
	}
	if readyReplicas(actual) != replicas(actual) {
		return r.report(ctx, f, h, "Provisioning", "Waiting for ready replicas; inspect Pod scheduling, PVC binding and runtime readiness. Capacity is externally provisioned", true)
	}
	return r.report(ctx, f, h, "Provisioned", "Initial infrastructure and runtime readiness observed; this is not production or durability qualification", true)
}

func (r *Reconciler) ensure(ctx context.Context, desired client.Object) error {
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !matches(desired, actual) {
		return fmt.Errorf("%T %s conflicts with required isolation/infrastructure; refusing adoption or mutation", actual, actual.GetName())
	}
	return nil
}

// Get must decode into an empty object: pre-populating it with desired fields can
// hide absent fields because JSON decoding may preserve fields missing on the wire.
func emptyObject(obj client.Object) client.Object {
	switch obj.(type) {
	case *appsv1.Deployment:
		return &appsv1.Deployment{}
	case *appsv1.StatefulSet:
		return &appsv1.StatefulSet{}
	case *corev1.Service:
		return &corev1.Service{}
	case *networkingv1.NetworkPolicy:
		return &networkingv1.NetworkPolicy{}
	case *policyv1.PodDisruptionBudget:
		return &policyv1.PodDisruptionBudget{}
	default:
		panic("unsupported managed resource type")
	}
}

// readyReplicas reports the workload's ready replica count. A status the workload
// controller has not caught up to yet counts as zero ready replicas.
func readyReplicas(w client.Object) int32 {
	var ready int32
	var observed int64
	switch w := w.(type) {
	case *appsv1.Deployment:
		ready, observed = w.Status.ReadyReplicas, w.Status.ObservedGeneration
	case *appsv1.StatefulSet:
		ready, observed = w.Status.ReadyReplicas, w.Status.ObservedGeneration
	}
	if observed < w.GetGeneration() {
		return 0
	}
	return ready
}

// report projects the hydrated journal h onto fleet status. h is this pass's
// single hydration, carrying the journal the pass has been deciding and writing
// against, so the projection describes what was just committed. Callers that
// never hydrated one — a configuration rejected before the reservation exists,
// a namespace the operator cannot read — pass nil and it is read here, once.
func (r *Reconciler) report(ctx context.Context, f *fleet.CelldFleet, h *hydratedJournal, reason, message string, provisioned bool) (ctrl.Result, error) {
	before := f.DeepCopy()
	f.Status.DesiredReplicas = f.Spec.Replicas
	f.Status.AppliedReplicas = 0
	if h == nil {
		h = &hydratedJournal{}
		loaded := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, types.NamespacedName{Name: reservationName(f)}, loaded); err == nil {
			h = r.hydrate(ctx, loaded)
		}
	}
	if res := h.res; res != nil {
		if j := h.j; h.err == nil && j != nil && res.Spec.FleetUID == string(f.UID) {
			f.Status.Lifecycle = fleet.LifecycleStatus{PossibleLoss: j.Loss}
			f.Status.Capacity = fleet.CapacityStatus{}
			if j.Capacity != nil && f.Spec.Capacity != nil {
				f.Status.Capacity = j.Capacity.Decision
				if (f.Spec.Capacity.Mode == "Automatic" || f.Spec.Capacity.Mode == "ScaleOut") && j.Capacity.Decision.DesiredReplicas > 0 {
					f.Status.DesiredReplicas = j.Capacity.Decision.DesiredReplicas
				}
			}

			if op := j.Operation; op != nil {
				f.Status.DesiredReplicas = op.To
				f.Status.Lifecycle = fleet.LifecycleStatus{OperationID: op.ID, Phase: op.Phase, From: op.From, To: op.To, TargetPod: op.TargetPod, TargetUID: op.TargetUID, TargetGeneration: op.TargetGeneration, PossibleLoss: j.Loss}
			}
			f.Status.Lifecycle.EvidenceBlocker = j.Inventory.Blocker
			// The shared observer's cryptographic/process-fencing diagnosis is
			// not a Bucket completion requirement. Previously admitted Bucket
			// generations remain accounted for; unknown history still warns.
			if f.Spec.Profile == "Bucket" && len(j.BucketHistory) > 0 && (j.Inventory.Blocker == "HistoricalSessionUnresolved" || j.Inventory.Blocker == "SessionBindingUnqualified") {
				covered := true
				for _, observed := range j.Inventory.Sessions {
					if !slices.ContainsFunc(j.BucketHistory, func(s bucketSession) bool {
						return s.Node == observed.Node && s.Generation == observed.Generation && observed.Epoch == 0
					}) {
						covered = false
					}
				}
				if covered {
					f.Status.Lifecycle.EvidenceBlocker = ""
				}
			}
			for _, session := range j.BucketHistory {
				if session.Retired {
					f.Status.Lifecycle.RetiredBucketSessions++
				}
			}
			f.Status.Lifecycle.SessionCount = int32(len(j.Inventory.Sessions))
			if !j.Inventory.CheckedAt.IsZero() {
				f.Status.Lifecycle.EvidenceCheckedAt = j.Inventory.CheckedAt.UTC().Format(time.RFC3339)
			}
			if j.Operation != nil {
				f.Status.Lifecycle.Stalled = j.Operation.Stalled
				if !j.Operation.StartedAt.IsZero() {
					f.Status.Lifecycle.StartedAt = j.Operation.StartedAt.UTC().Format(time.RFC3339)
				}
				if !j.Operation.Deadline.IsZero() {
					f.Status.Lifecycle.Deadline = j.Operation.Deadline.UTC().Format(time.RFC3339)
				}
			}
			if m := j.Maintenance; m != nil {
				if m.Kind == "Delete" {
					f.Status.DesiredReplicas = 0
				}
				f.Status.Lifecycle.OperationID = m.ID
				f.Status.Lifecycle.Phase = m.Phase
				f.Status.Lifecycle.Stalled = !m.Deadline.IsZero() && !r.capacityNow().Before(m.Deadline)
				if !m.StartedAt.IsZero() {
					f.Status.Lifecycle.StartedAt = m.StartedAt.UTC().Format(time.RFC3339)
				}
				if !m.Deadline.IsZero() {
					f.Status.Lifecycle.Deadline = m.Deadline.UTC().Format(time.RFC3339)
				}
			}
			if len(j.History) > 0 {
				last := j.History[len(j.History)-1]
				f.Status.Lifecycle.LastOutcome = last.Outcome
				if !last.EvidenceAt.IsZero() {
					f.Status.Lifecycle.LastCompletionAt = last.EvidenceAt.UTC().Format(time.RFC3339)
				}
			}
			if request := j.Request; request != nil {
				f.Status.Lifecycle.RequestKind = request.Kind
				f.Status.Lifecycle.RequestID = request.ID
				f.Status.Lifecycle.TargetImage = request.TargetImage
			}
		}
	}
	if provisioned {
		f.Status.Reservation = reservationName(f)
	}
	f.Status.ObservedGeneration = f.Generation
	observed := emptyObject(workload(f, r.Options))
	var ready int32
	serving := false
	// A forbidden read means the fleet namespace lacks the operator Role; the
	// caller reports that, and readiness is simply unknown (zero) meanwhile.
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), observed); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return ctrl.Result{}, err
	} else if err == nil && observed.GetLabels()[FleetLabel] == string(f.UID) && observed.GetDeletionTimestamp().IsZero() {
		f.Status.AppliedReplicas = replicas(observed)
		ready = readyReplicas(observed)
		serving = ready > 0 && ready == replicas(observed)
	}
	f.Status.ReadyReplicas = ready
	r.observeReplicaCounts(ctx, f)
	set := func(kind string, yes bool, why, msg string) {
		status := metav1.ConditionFalse
		if yes {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: kind, Status: status, Reason: why, Message: msg, ObservedGeneration: f.Generation})
	}
	set("MaintenancePaused", paused(f), reason, message)
	set("Deleting", !f.DeletionTimestamp.IsZero(), reason, message)
	set("Ready", serving, reason, message)
	set("InfrastructureReady", provisioned || reason == "LifecycleProgress", reason, message)
	set("Progressing", reason == "LifecycleProgress" || reason == "Provisioning", reason, message)
	set("Blocked", !provisioned && reason != "LifecycleProgress", reason, message)
	set("LifecycleBlocked", true, "QualificationIncomplete", "Production automatic contraction, runtime upgrades and uncertain-node recovery remain unqualified; experimental maintenance requires exact runtime and storage evidence")
	set("ProductionQualified", false, "QualificationIncomplete", "Local prototype only; AWS, retained-EBS recovery, fencing, follower AZ diversity and restart safety remain unqualified")
	// Both journal budgets fail closed, and a journal that reaches either one can
	// no longer be written at all. Warn at half of each; block on neither.
	footprint := journalFootprint{}
	if h.res != nil {
		footprint = measureJournal(h.res)
	}
	set("JournalSizeWarning", footprint.nearCapacity(), journalSizeReason(footprint), journalSizeMessage(footprint))
	if meta.IsStatusConditionTrue(f.Status.Conditions, "Blocked") {
		if f.Status.BlockedSince == "" || !meta.IsStatusConditionTrue(before.Status.Conditions, "Blocked") {
			f.Status.BlockedSince = r.capacityNow().UTC().Format(time.RFC3339)
		}
	} else {
		f.Status.BlockedSince = ""
	}
	if !equality.Semantic.DeepEqual(before.Status, f.Status) {
		if err := r.Status().Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.recordConditionChange(f, before.Status.Conditions)
	publishFleetMetrics(f, footprint, r.capacityNow())
	return ctrl.Result{RequeueAfter: reconcileDelay(f)}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("celld-operator")
	}
	return ctrl.NewControllerManagedBy(mgr).For(&fleet.CelldFleet{}).WithOptions(fleetControllerOptions()).Complete(r)
}

func fleetControllerOptions() controlleroptions.Options {
	return controlleroptions.Options{MaxConcurrentReconciles: 4}
}
