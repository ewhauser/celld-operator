// Package controller provisions experimental fleet infrastructure with durable manual lifecycle intent.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const attemptAnnotation = "celld.eric.dev/workload-creation-attempted"

// Reconciler uses an uncached client for the durable reservation and creation intent.
// Kubernetes Create is the cross-controller arbitration point; leader election is not the safety proof.
type Reconciler struct {
	client.Client
	Options               Options
	NetworkPolicyEnforced bool
	Recorder              events.EventRecorder
	launcherCall          func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error)
	Collector             capacityCollector
	ApplicationRuntime    controlplane.ApplicationReader
	now                   func() time.Time
}

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func reservationSpecJSON(f *fleet.CelldFleet) []byte {
	spec := f.Spec
	// Preserve reservation hashes from before the opt-in Ordered layout existed.
	if spec.BucketWorkload == "Deployment" {
		spec.BucketWorkload = ""
	}
	spec.Previews = nil
	spec.Capacity = nil
	spec.Routing = nil
	spec.RuntimeImage = ""
	spec.Maintenance = nil
	b, _ := json.Marshal(spec)
	return b
}
func specHash(f *fleet.CelldFleet) string { return digest(reservationSpecJSON(f)) }
func legacyQualificationSpecHash(f *fleet.CelldFleet) string {
	b := reservationSpecJSON(f)
	// v0.0.2 serialized this required field immediately before profile. Restore
	// exactly that JSON layout without changing hashes written for new fleets.
	marker := []byte(`"profile":`)
	i := bytes.Index(b, marker)
	if i < 0 {
		return ""
	}
	legacy := make([]byte, 0, len(b)+len(`"qualification":"Experimental",`))
	legacy = append(legacy, b[:i]...)
	legacy = append(legacy, []byte(`"qualification":"Experimental",`)...)
	legacy = append(legacy, b[i:]...)
	return digest(legacy)
}
func reservationName(f *fleet.CelldFleet) string {
	if f.Spec.Storage.Prefix != "" {
		return "s3-scope-" + digest([]byte(f.Spec.Storage.Bucket + "\x00" + f.Spec.Storage.Prefix))[:54]
	}
	return bucketReservationName(f.Spec.Storage.Bucket)
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
	if err == nil {
		err = r.reportRouting(ctx, req.NamespacedName)
		if result.RequeueAfter == 0 {
			result.RequeueAfter = reconcileDelay(f)
		}
	}
	if err == nil && r.ApplicationRuntime != nil {
		err = r.reportApplication(ctx, req.NamespacedName)
	}
	return result, err
}

func (r *Reconciler) reconcileFleet(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	f.Default()
	if !f.DeletionTimestamp.IsZero() {
		return r.deleteFleet(ctx, f)
	}
	if err := f.ValidateRuntime(); err != nil {
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
		if sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete || sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || !r.supportedCSI(sc.Provisioner) {
			return r.report(ctx, f, nil, "InvalidStorageClass", "Requires EBS CSI, Delete reclaim policy and WaitForFirstConsumer binding (local test permits the hostpath CSI driver)", false)
		}
	}
	if err := verifySharedStorage(ctx, r.Client, f); err != nil {
		return r.report(ctx, f, nil, "StorageScopeConflict", err.Error(), false)
	}
	reservation := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)}
	expected := reservation.Spec
	if f.Spec.Storage.Initialization != nil {
		var err error
		reservation, err = ensureSeedReservation(ctx, r.Client, f)
		if err != nil {
			return r.report(ctx, f, nil, "SeedReservationBlocked", err.Error(), false)
		}
	} else if err := r.Create(ctx, reservation); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: reservation.Name}, reservation); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Load the one bounded current operation once; status is a projection.
	h := r.hydrate(ctx, reservation)
	if h.err != nil {
		return r.report(ctx, f, h, "OperationInvalid", h.err.Error(), false)
	}
	if len(reservation.OwnerReferences) != 0 || !reservation.DeletionTimestamp.IsZero() || !r.reservationMatches(ctx, f, h, expected) {
		return r.report(ctx, f, h, "StorageScopeConflict", "Bucket is permanently reserved to another fleet UID or immutable configuration; no resources adopted", false)
	}

	if ready, err := r.gateSeed(ctx, f, reservation); !ready || err != nil {
		message := "Waiting for all seed objects to be imported"
		if err != nil {
			message = err.Error()
		}
		return r.report(ctx, f, h, "SeedInitializing", message, false)
	}
	for _, obj := range prerequisites(f, r.Options) {
		if err := r.ensure(ctx, obj); err != nil {
			// A lost field-upgrade race retries from a fresh read instead of
			// reporting a conflict the next verification may not find.
			if apierrors.IsConflict(err) {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
		}
	}
	desired := workload(f, r.Options)
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		if !knownRuntime(runtimeImage(f)) || r.Options.LauncherImage == "" {
			return r.report(ctx, f, h, "UnsupportedTransition", "Provisioning requires a verified compatible fork digest and trusted launcher; there is no default compatible image", false)
		}
		if reservation.Annotations[attemptAnnotation] != "" {
			return r.report(ctx, f, h, "LifecycleBlocked", "Workload is missing after a recorded creation attempt; automatic recreation could reuse an unsafe identity or disk", false)
		}
		if r.Options.LauncherImage != "" {
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
	// In automatic mode, the current state owns applied capacity, while spec.replicas
	// remains the user's manual command field. Never reconcile it back implicitly.
	if f.Spec.Capacity != nil {
		setReplicas(desired, replicas(actual))
	}
	if !matches(desired, actual) {
		return r.report(ctx, f, h, "LifecycleBlocked", "Existing workload differs from the recorded spec; no rollout, adoption or drift repair is authorized", false)
	}
	if reservation.Annotations[attemptAnnotation] == "" {
		return r.report(ctx, f, h, "LifecycleBlocked", "Existing workload has no creation intent; refusing adoption", false)
	}
	// Maintenance validates each admitted Pod before trusting its proof. Check the
	// same contract now so an incompatible admission mutation is reported while
	// the fleet is steady, not first discovered by a maintenance request.
	if s := h.j; s != nil {
		pods, err := r.currentPods(ctx, f, s)
		if err != nil {
			return r.report(ctx, f, h, "PodCompositionUnsupported", err.Error(), false)
		}
		for i := range pods {
			if p := &pods[i]; p.DeletionTimestamp.IsZero() {
				if err := validatePersistentPod(appliedRuntime(f, s), p, r.Options); err != nil {
					return r.report(ctx, f, h, "PodCompositionUnsupported", fmt.Sprintf("Pod %s cannot be maintained: %v", p.Name, err), false)
				}
			}
		}
	}
	if readyReplicas(actual) != replicas(actual) {
		return r.report(ctx, f, h, "Provisioning", "Waiting for ready replicas; inspect Pod scheduling, PVC binding and runtime readiness. Capacity is externally provisioned", true)
	}
	return r.report(ctx, f, h, "Provisioned", "Initial infrastructure and runtime readiness observed", true)
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
	if matches(desired, actual) {
		return nil
	}
	// The only mutation authorized here: an object that already matches except for
	// a field a newer release declares and the live object leaves unset. matches
	// still requires this fleet's label, no owners and no deletion.
	upgraded := backfill(desired, actual)
	if upgraded == nil {
		return fmt.Errorf("%T %s conflicts with required isolation/infrastructure; refusing adoption or mutation", actual, actual.GetName())
	}
	// upgraded is the live object plus only the filled field, so allocated
	// addresses and server defaults go back unchanged. Update carries the
	// resourceVersion just verified: any concurrent writer turns this into a
	// Conflict and the next reconcile verifies from scratch.
	if err := r.Update(ctx, upgraded); err != nil {
		if apierrors.IsForbidden(err) {
			// Not NamespaceAccessDenied: admission policy also answers Forbidden.
			return fmt.Errorf("%T %s field upgrade refused; the fleet-namespace Role from config/rbac/fleet-namespace.yaml must grant update: %w", actual, actual.GetName(), err)
		}
		return err
	}
	// Re-read into an empty object rather than trusting the decoded Update
	// response (see emptyObject). An admission rewrite is refused, not adopted.
	verified := emptyObject(desired)
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), verified); err != nil {
		return err
	}
	if !matches(desired, verified) {
		return fmt.Errorf("%T %s does not match required isolation/infrastructure after field upgrade; refusing further mutation", verified, verified.GetName())
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

// report rebuilds status from current Kubernetes authority. Status is never an input.
func (r *Reconciler) report(ctx context.Context, f *fleet.CelldFleet, h *loadedState, reason, message string, provisioned bool) (ctrl.Result, error) {
	before := f.DeepCopy()
	f.Status.DesiredReplicas = f.Spec.Replicas
	f.Status.AppliedReplicas = 0
	if h == nil {
		h = &loadedState{}
		loaded := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, types.NamespacedName{Name: reservationName(f)}, loaded); err == nil {
			h = r.hydrate(ctx, loaded)
		}
	}
	f.Status.Lifecycle = fleet.LifecycleStatus{}
	f.Status.Capacity = fleet.CapacityStatus{}
	if s := h.j; h.err == nil && s != nil && s.FleetUID == f.UID {
		if s.Capacity != nil && f.Spec.Capacity != nil {
			f.Status.Capacity = s.Capacity.Decision
			if (f.Spec.Capacity.Mode == "Automatic" || f.Spec.Capacity.Mode == "ScaleOut") && s.Capacity.Decision.DesiredReplicas > 0 {
				f.Status.DesiredReplicas = s.Capacity.Decision.DesiredReplicas
			}
		}
		if s.Completion != nil {
			f.Status.Lifecycle.LastOutcome = s.Completion.Outcome
			f.Status.Lifecycle.LastCompletionAt = s.Completion.At.UTC().Format(time.RFC3339)
		}
		if o := s.Operation; o != nil {
			f.Status.DesiredReplicas = o.To
			f.Status.Lifecycle.OperationID = o.ID
			f.Status.Lifecycle.Phase = o.Phase
			f.Status.Lifecycle.From = o.From
			f.Status.Lifecycle.To = o.To
			f.Status.Lifecycle.RequestID = o.ID
			f.Status.Lifecycle.RequestKind = o.Kind
			f.Status.Lifecycle.TargetImage = o.TargetImage
			f.Status.Lifecycle.Blocker = o.Blocker
			f.Status.Lifecycle.StartedAt = o.StartedAt.UTC().Format(time.RFC3339)
			f.Status.Lifecycle.Deadline = o.Deadline.UTC().Format(time.RFC3339)
			f.Status.Lifecycle.Stalled = !r.capacityNow().Before(o.Deadline)
			if len(o.Targets) > 0 {
				t := o.Targets[0]
				f.Status.Lifecycle.TargetPod = t.Pod
				f.Status.Lifecycle.TargetUID = string(t.PodUID)
				f.Status.Lifecycle.TargetGeneration = t.Identity.Generation
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
	// Clear obsolete qualification projections on fleets created by older releases.
	meta.RemoveStatusCondition(&f.Status.Conditions, "LifecycleBlocked")
	meta.RemoveStatusCondition(&f.Status.Conditions, "ProductionQualified")
	footprint := stateFootprint{}
	if h.res != nil {
		footprint = measureState(h.res)
	}
	set("OperationSizeWarning", footprint.nearCapacity(), "BoundedOperation", stateSizeMessage(footprint))
	set("DiskCleanupPending", diskCleanupPending(h.j), "CSIDeletionPending", "Current strict proof remains reserved until exact PVC/PV deletion and attachment removal are observed")
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
	return ctrl.NewControllerManagedBy(mgr).For(&fleet.CelldFleet{}).
		Watches(&fleet.CelldStorageReservation{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
			seed := o.(*fleet.CelldStorageReservation)
			if seed.Spec.Initialization == nil {
				return nil
			}
			return []ctrl.Request{{Namespace: seed.Spec.FleetNamespace, Name: seed.Spec.Initialization.Target.FleetName}}
		})).WithOptions(fleetControllerOptions()).Complete(r)
}

func fleetControllerOptions() controlleroptions.Options {
	return controlleroptions.Options{MaxConcurrentReconciles: 4}
}
