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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const attemptAnnotation = "celld.example.com/workload-creation-attempted"

// Reconciler uses an uncached client for the durable reservation and creation journal.
// Kubernetes Create is the cross-controller arbitration point; leader election is not the safety proof.
type Reconciler struct {
	client.Client
	Options               Options
	NetworkPolicyEnforced bool
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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	f.Default()
	if !f.DeletionTimestamp.IsZero() {
		return r.deleteFleet(ctx, f)
	}
	if err := f.Validate(); err != nil {
		return r.report(ctx, f, "InvalidConfiguration", err.Error(), 0, false)
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
		return r.report(ctx, f, "IsolationUnverified", "Administrator must verify a NetworkPolicy enforcing CNI and enable --network-policy-enforced before provisioning", 0, false)
	}
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Spec.ServiceAccountName}, sa); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, "ServiceAccountMissing", "Referenced ServiceAccount does not exist in the fleet namespace", 0, false)
	}
	if f.Spec.Profile == "PersistentFleet" {
		sc := &storagev1.StorageClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: f.Spec.Storage.StorageClassName}, sc); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, "StorageClassMissing", "Referenced StorageClass does not exist", 0, false)
		}
		if sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain || sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || (!r.Options.LocalTest && sc.Provisioner != "ebs.csi.aws.com") {
			return r.report(ctx, f, "InvalidStorageClass", "Requires EBS CSI, Retain reclaim policy and WaitForFirstConsumer binding (local test permits a different provisioner)", 0, false)
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
	if len(reservation.OwnerReferences) != 0 || !reservation.DeletionTimestamp.IsZero() || !r.reservationMatches(ctx, f, reservation, expected) {
		return r.report(ctx, f, "StorageScopeConflict", "Bucket is permanently reserved to another fleet UID or immutable configuration; no resources adopted", 0, false)
	}

	for _, obj := range prerequisites(f, r.Options) {
		if err := r.ensure(ctx, obj); err != nil {
			return r.report(ctx, f, "InfrastructureBlocked", err.Error(), 0, false)
		}
	}
	desired := workload(f, r.Options)
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		if f.Spec.RuntimeImage != "" && f.Spec.RuntimeImage != Image {
			return r.report(ctx, f, "UnsupportedTransition", "Initial runtime must use the qualified adapter pin; no alternate image is supported", 0, false)
		}
		if reservation.Annotations[attemptAnnotation] != "" {
			return r.report(ctx, f, "LifecycleBlocked", "Workload is missing after a recorded creation attempt; automatic recreation could reuse an unsafe identity or disk", 0, false)
		}
		if f.Spec.Profile == "PersistentFleet" && r.Options.LauncherImage != "" {
			if err := r.createLauncherKey(ctx, f, reservation); err != nil {
				return r.report(ctx, f, "LauncherIdentityBlocked", err.Error(), 0, false)
			}
		}
		claims := initialClaims(f, desired)
		if err := r.checkInitialClaims(ctx, claims); err != nil {
			return r.report(ctx, f, "StorageIdentityConflict", err.Error(), 0, false)
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
				return r.report(ctx, f, "StorageIdentityConflict", fmt.Sprintf("Cannot exclusively create PVC %s: %v; retained claims require manual review", claim.Name, err), 0, false)
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
		return r.report(ctx, f, "Provisioning", "Initial workload created; waiting for runtime readiness and placement", 0, true)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if result, handled, err := r.lifecycle(ctx, f, reservation, actual); handled || err != nil {
		return result, err
	}
	// In automatic mode, the journal owns applied capacity, while spec.replicas
	// remains the user's manual command field. Never reconcile it back implicitly.
	if f.Spec.Capacity != nil {
		setReplicas(desired, replicas(actual))
	}
	if !matches(desired, actual) {
		return r.report(ctx, f, "LifecycleBlocked", "Existing workload differs from the journaled spec; no rollout, adoption or drift repair is authorized", 0, false)
	}
	if reservation.Annotations[attemptAnnotation] == "" {
		return r.report(ctx, f, "LifecycleBlocked", "Existing workload has no creation journal; refusing adoption", 0, false)
	}
	var ready int32
	var observed int64
	switch w := actual.(type) {
	case *appsv1.Deployment:
		ready = w.Status.ReadyReplicas
		observed = w.Status.ObservedGeneration
	case *appsv1.StatefulSet:
		ready = w.Status.ReadyReplicas
		observed = w.Status.ObservedGeneration
	}
	if observed < actual.GetGeneration() {
		ready = 0
	}
	if ready != replicas(actual) {
		return r.report(ctx, f, "Provisioning", "Waiting for ready replicas; inspect Pod scheduling, PVC binding and runtime readiness. Capacity is externally provisioned", ready, true)
	}
	return r.report(ctx, f, "Provisioned", "Initial infrastructure and runtime readiness observed; this is not production or durability qualification", ready, true)
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

func (r *Reconciler) report(ctx context.Context, f *fleet.CelldFleet, reason, message string, ready int32, provisioned bool) (ctrl.Result, error) {
	before := f.DeepCopy()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(ctx, types.NamespacedName{Name: reservationName(f)}, res); err == nil {
		if j, err := readJournal(res); err == nil && j != nil && res.Spec.FleetUID == string(f.UID) {
			f.Status.Lifecycle = fleet.LifecycleStatus{PossibleLoss: j.Loss}
			f.Status.Capacity = fleet.CapacityStatus{}
			if j.Capacity != nil && f.Spec.Capacity != nil {
				f.Status.Capacity = j.Capacity.Decision
			}

			if op := j.Operation; op != nil {
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
				if !j.Operation.Deadline.IsZero() {
					f.Status.Lifecycle.Deadline = j.Operation.Deadline.UTC().Format(time.RFC3339)
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
	serving := false
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), observed); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	} else if err == nil && observed.GetLabels()[FleetLabel] == string(f.UID) && observed.GetDeletionTimestamp().IsZero() {
		var generation int64
		switch w := observed.(type) {
		case *appsv1.Deployment:
			ready, generation = w.Status.ReadyReplicas, w.Status.ObservedGeneration
		case *appsv1.StatefulSet:
			ready, generation = w.Status.ReadyReplicas, w.Status.ObservedGeneration
		}
		if generation < observed.GetGeneration() {
			ready = 0
		}
		serving = ready > 0 && ready == replicas(observed)
	} else {
		ready = 0
	}
	f.Status.ReadyReplicas = ready
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
	set("LifecycleBlocked", true, "QualificationIncomplete", "Production automatic contraction and uncertain-node PersistentFleet recovery remain unqualified; upgrades, restarts and deletion are blocked")
	set("ProductionQualified", false, "QualificationIncomplete", "Local prototype only; AWS, retained-EBS recovery, fencing, follower AZ diversity and restart safety remain unqualified")
	if !equality.Semantic.DeepEqual(before.Status, f.Status) {
		if err := r.Status().Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&fleet.CelldFleet{}).WithOptions(fleetControllerOptions()).Complete(r)
}

func fleetControllerOptions() controlleroptions.Options {
	return controlleroptions.Options{MaxConcurrentReconciles: 4}
}
