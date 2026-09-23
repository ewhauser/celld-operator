package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const previewFinalizer = "celld.eric.dev/preview"
const previewCreated = "celld.eric.dev/preview-fleet-created"
const previewParentFleetUID = "celld.eric.dev/preview-parent-fleet-uid"
const previewSeedReservation = "celld.eric.dev/preview-seed-reservation"
const previewURL = "celld.eric.dev/preview-url"

// PreviewReconciler delegates all runtime and storage authority to CelldFleet.
type PreviewReconciler struct {
	client.Client
	now func() time.Time
}

func previewName(p *fleet.CelldPreview) string {
	return "p-" + strings.ReplaceAll(string(p.UID), "-", "")
}

func previewFleet(p *fleet.CelldPreview, pool *fleet.CelldFleet) *fleet.CelldFleet {
	// Preserve the entire Kubernetes UID: names and URLs cannot collide across
	// namespaces or when a deleted preview name is reused.
	name := previewName(p)
	f := &fleet.CelldFleet{Name: name, Namespace: p.Namespace, Spec: pool.PreviewFleetSpec(name)}
	r := pool.Spec.Previews.Routing.DeepCopy()
	f.Spec.Routing = &fleet.RoutingSpec{Hostnames: []fleet.RouteHostname{fleet.RouteHostname(name + "." + r.BaseDomain)}, Source: r.Source, Gateway: r.Gateway, Ingress: r.Ingress}
	owner := metav1.NewControllerRef(p, fleet.GroupVersion.WithKind("CelldPreview"))
	owner.BlockOwnerDeletion = new(false)
	f.OwnerReferences = []metav1.OwnerReference{*owner}
	f.Finalizers = []string{Finalizer}
	f.Default()
	return f
}

func previewOwned(p *fleet.CelldPreview, f *fleet.CelldFleet) bool {
	o := metav1.GetControllerOf(f)
	return o != nil && o.UID == p.UID && o.Name == p.Name && o.Kind == "CelldPreview" && o.APIVersion == fleet.GroupVersion.String()
}

func (r *PreviewReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p := &fleet.CelldPreview{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	ttl := p.Spec.TTLSeconds
	if ttl == 0 {
		ttl = 86400
	}
	expires := p.CreationTimestamp.Add(time.Duration(ttl) * time.Second)
	desired := &fleet.CelldFleet{Name: previewName(p), Namespace: p.Namespace}
	expired := !now.Before(expires)
	deleting := !p.DeletionTimestamp.IsZero()
	actual := &fleet.CelldFleet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return ctrl.Result{}, err
	}
	if !missing && !previewOwned(p, actual) {
		return r.report(ctx, p, desired, expires, "Blocked", "OwnershipConflict", "Generated fleet name is occupied by a different owner", false)
	}
	if !missing {
		desired = actual.DeepCopy()
	}
	if deleting || expired {
		stopped, seedErr := r.cancelPreviewSeed(ctx, p, actual, missing)
		if seedErr != nil {
			return r.report(ctx, p, desired, expires, "Deleting", "SeedCancellationBlocked", seedErr.Error(), false)
		}
		if !stopped {
			return r.report(ctx, p, desired, expires, "Deleting", "SeedCanceling", "Waiting for the seed executor to stop all writes", false)
		}
		if !missing {
			if actual.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, actual, client.Preconditions{UID: new(actual.UID), ResourceVersion: new(actual.ResourceVersion)}); err != nil {
					return ctrl.Result{}, client.IgnoreNotFound(err)
				}
			}
			message := "Waiting for safe fleet shutdown; preview prefix data and reservations are retained"
			if c := meta.FindStatusCondition(actual.Status.Conditions, "Ready"); c != nil {
				message += ": " + c.Message
			}
			return r.report(ctx, p, desired, expires, "Deleting", "FleetDeleting", message, false)
		}
		if deleting {
			before := p.DeepCopy()
			controllerutil.RemoveFinalizer(p, previewFinalizer)
			return ctrl.Result{}, r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		return r.report(ctx, p, desired, expires, "Expired", "LifetimeExceeded", "Preview expired; preview prefix data and reservations are retained", false)
	}
	if ttl < 60 || ttl > 604800 || p.UID == "" || p.CreationTimestamp.IsZero() || p.Spec.Source == "" || len(p.Spec.Source) > 256 || len(p.Spec.Revision) > 256 || len(validation.IsDNS1123Subdomain(p.Spec.FleetRef.Name)) != 0 {
		return r.report(ctx, p, desired, expires, "Blocked", "InvalidConfiguration", "Invalid preview source, identity, lifetime or pool reference", false)
	}
	if err := p.Spec.Seed.Validate(); err != nil {
		return r.report(ctx, p, desired, expires, "Blocked", "InvalidSeed", err.Error(), false)
	}
	pool := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.FleetRef.Name}, pool); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, p, desired, expires, "Blocked", "PoolMissing", "Referenced preview pool does not exist in this namespace", false)
	}
	if !pool.DeletionTimestamp.IsZero() || pool.UID == "" {
		return r.report(ctx, p, desired, expires, "Blocked", "PoolUnavailable", "Preview pool is deleting or has no identity", false)
	}
	if uid := p.Annotations[previewParentFleetUID]; uid != "" && uid != string(pool.UID) {
		return r.report(ctx, p, desired, expires, "Blocked", "PoolIdentityChanged", "A recreated pool cannot adopt an existing preview", false)
	}
	if pool.Spec.Previews == nil {
		return r.report(ctx, p, desired, expires, "Blocked", "PreviewsDisabled", "Referenced fleet has no preview configuration", false)
	}
	if pool.Spec.Previews.Storage.Bucket == pool.Spec.Storage.Bucket {
		return r.report(ctx, p, desired, expires, "Blocked", "StoragePoolConflict", "Preview storage must use a separate bucket from the parent runtime", false)
	}
	desired = previewFleet(p, pool)
	routing := pool.Spec.Previews.Routing
	scheme := routing.Scheme
	if scheme == "" {
		scheme = "https"
	}
	if !fleet.ValidRuntimeImage(pool.Spec.Previews.RuntimeImage) || len(validation.IsDNS1123Subdomain(routing.BaseDomain)) != 0 || len(routing.BaseDomain) > 218 || (scheme != "http" && scheme != "https") || scheme == "https" && routing.Ingress != nil && routing.Ingress.TLSSecretName == "" {
		return r.report(ctx, p, desired, expires, "Blocked", "InvalidConfiguration", "Pool requires a runtime digest, valid domain and routing scheme; HTTPS ingress requires a TLS secret", false)
	}
	if err := desired.Validate(); err != nil {
		return r.report(ctx, p, desired, expires, "Blocked", "InvalidConfiguration", err.Error(), false)
	}
	if err := reservePreviewPool(ctx, r.Client, pool); err != nil {
		return r.report(ctx, p, desired, expires, "Blocked", "StoragePoolConflict", err.Error(), false)
	}
	if !controllerutil.ContainsFinalizer(p, previewFinalizer) {
		before := p.DeepCopy()
		controllerutil.AddFinalizer(p, previewFinalizer)
		if err := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if p.Spec.Seed != nil {
		request, err := r.previewSeedRequest(ctx, p, pool, desired, expires)
		if err != nil {
			return r.report(ctx, p, desired, expires, "Blocked", "SeedBlocked", err.Error(), false)
		}
		desired.Spec.Storage.Initialization = request
	}

	if missing {
		// Persist intent before creation. Never silently reuse a bucket after loss
		// of a child object, even if status has been cleared or the manager restarted.
		if p.Annotations[previewCreated] != "" {
			return r.report(ctx, p, desired, expires, "Blocked", "FleetMissing", "Fleet missing after creation intent; inspect retained storage before creating a new preview", false)
		}
		before := p.DeepCopy()
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[previewCreated] = "true"
		if p.Spec.Seed != nil {
			p.Annotations[previewSeedReservation] = reservationName(desired)
		}
		p.Annotations[previewParentFleetUID] = string(pool.UID)
		p.Annotations[previewURL] = scheme + "://" + desired.Name + "." + routing.BaseDomain
		if err := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
				// A definite API rejection created no child. Allow retry after
				// fixing namespace RBAC/quota; ambiguous transport failures keep intent.
				before := p.DeepCopy()
				delete(p.Annotations, previewCreated)
				delete(p.Annotations, previewSeedReservation)
				if patchErr := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				return r.report(ctx, p, desired, expires, "Blocked", "FleetCreationRejected", "Check namespace fleet RBAC, quota and admission: "+err.Error(), false)
			}
			return ctrl.Result{}, err
		}
		if p.Spec.Seed != nil {
			return r.report(ctx, p, desired, expires, "Initializing", "SeedPending", "Waiting for executor snapshots and all object imports", false)
		}
		return r.report(ctx, p, desired, expires, "Pending", "FleetCreated", "Waiting for fleet provisioning and route acceptance", false)
	}
	if !actual.DeletionTimestamp.IsZero() {
		return r.report(ctx, p, desired, expires, "Deleting", "FleetDeleting", "The preview fleet is being deleted", false)
	}
	if !equality.Semantic.DeepEqual(actual.Spec, desired.Spec) {
		return r.report(ctx, p, desired, expires, "Blocked", "FleetDrift", "Preview fleet configuration differs; no automatic adoption or rollout", false)
	}
	// Missing namespace RBAC or prerequisites must be visible while the executor
	// waits for a destination reservation; otherwise initialization looks idle.
	if c := meta.FindStatusCondition(actual.Status.Conditions, "Blocked"); c != nil && c.ObservedGeneration == actual.Generation && c.Status == metav1.ConditionTrue && c.Reason != "SeedInitializing" {
		return r.report(ctx, p, desired, expires, "Blocked", c.Reason, c.Message, false)
	}
	if p.Spec.Seed != nil {
		seed := &fleet.CelldStorageReservation{}
		err := r.Get(ctx, client.ObjectKey{Name: reservationName(actual)}, seed)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if seed.Status.Canceled || seed.Status.Phase == "Failed" || seed.Status.Phase == "Canceled" {
			return r.report(ctx, p, desired, expires, "Blocked", "SeedFailed", "Seed initialization stopped: "+seed.Status.Message, false)
		}
		if seed.Status.Phase != "Succeeded" {
			return r.report(ctx, p, desired, expires, "Initializing", "SeedPending", "Waiting for executor snapshots and all object imports", false)
		}
	}

	if c := meta.FindStatusCondition(actual.Status.Conditions, "Blocked"); c != nil && c.ObservedGeneration == actual.Generation && c.Status == metav1.ConditionTrue {
		return r.report(ctx, p, desired, expires, "Blocked", c.Reason, c.Message, false)
	}
	for _, kind := range []string{"Ready", "RoutingReady"} {
		c := meta.FindStatusCondition(actual.Status.Conditions, kind)
		if c == nil || c.ObservedGeneration != actual.Generation {
			return r.report(ctx, p, desired, expires, "Pending", "AwaitingFleet", "Waiting for current fleet and route observations", false)
		}
		if c.Status != metav1.ConditionTrue {
			return r.report(ctx, p, desired, expires, "Pending", c.Reason, c.Message, false)
		}
	}
	return r.report(ctx, p, desired, expires, "Ready", "InfrastructureReady", "Fleet and route are ready; deploy the application and verify DNS, TLS and HTTP separately", true)
}

func (r *PreviewReconciler) report(ctx context.Context, p *fleet.CelldPreview, f *fleet.CelldFleet, expires time.Time, phase, reason, message string, ready bool) (ctrl.Result, error) {
	before := p.DeepCopy()
	if p.Spec.Seed != nil && f.Spec.Storage.Initialization != nil {
		p.Status.SeedReservation = reservationName(f)
		p.Status.SeedPhase = "Pending"
		seed := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKey{Name: p.Status.SeedReservation}, seed); err == nil && seed.Spec.Initialization != nil && seed.Spec.Initialization.Target.PreviewUID == string(p.UID) && seed.Status.Phase != "" {
			p.Status.SeedPhase = seed.Status.Phase
		}
	}

	p.Status.ObservedGeneration = p.Generation
	p.Status.FleetName = f.Name
	p.Status.ParentFleetName = p.Spec.FleetRef.Name
	p.Status.ParentFleetUID = p.Annotations[previewParentFleetUID]
	p.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc:8080", f.Name, f.Namespace)
	if u := p.Annotations[previewURL]; u != "" {
		p.Status.URL = u
	}
	if f.Spec.Storage.Bucket != "" {
		p.Status.StorageURL = f.Spec.Storage.URL()
	}
	p.Status.ExpiresAt = new(metav1.NewTime(expires))
	p.Status.Phase = phase
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: p.Generation})
	if !equality.Semantic.DeepEqual(before.Status, p.Status) {
		if err := r.Status().Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if phase == "Expired" {
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	delay := 10 * time.Second
	if remaining := expires.Sub(now); remaining > 0 && remaining < delay {
		delay = remaining
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}

func (r *PreviewReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&fleet.CelldPreview{}).Owns(&fleet.CelldFleet{}).
		Watches(&fleet.CelldStorageReservation{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
			seed := o.(*fleet.CelldStorageReservation)
			if seed.Spec.Initialization == nil {
				return nil
			}
			return []ctrl.Request{{Namespace: seed.Spec.FleetNamespace, Name: seed.Spec.Initialization.Target.PreviewName}}
		})).
		Watches(&fleet.CelldFleet{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrl.Request {
			if object.(*fleet.CelldFleet).Spec.Previews == nil {
				return nil
			}
			var previews fleet.CelldPreviewList
			if err := r.List(ctx, &previews, client.InNamespace(object.GetNamespace())); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "list previews for pool change")
				return nil
			}
			var requests []ctrl.Request
			for _, p := range previews.Items {
				if p.Spec.FleetRef.Name == object.GetName() {
					requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
				}
			}
			return requests
		})).Complete(r)
}
