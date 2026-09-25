package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Bucket fleets run CELLD_DURABILITY=bucket: celld acknowledges no write before
// the object store holds it, so a member's disk is a cache and losing any one
// member at any time loses no acknowledged write. They need no launcher, no
// strict shutdown proof and no current operation (ADR 0023). The workload
// controller performs scaling, restart and upgrade; the operator renders the
// desired objects and keeps capacity-policy history.

// bucketLoaded discards 0022 operation state a Bucket fleet no longer uses.
// Invalid or foreign state is dropped rather than blocking: nothing in it can
// make a Bucket change unsafe, and the next save replaces it.
func (r *Reconciler) bucketLoaded(f *fleet.CelldFleet, h *loadedState) *loadedState {
	out := &loadedState{res: h.res, j: h.j}
	if h.err != nil || (h.j != nil && h.j.FleetUID != f.UID) {
		out.j = nil
	}
	if s := out.j; s != nil && s.Operation != nil {
		s = new(*s)
		s.Completion = &operationCompletion{ID: s.Operation.ID, Kind: s.Operation.Kind, Outcome: "Superseded", At: r.capacityNow()}
		s.Operation = nil
		out.j = s
	}
	return out
}

func (r *Reconciler) reconcileBucket(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (ctrl.Result, error) {
	for _, obj := range prerequisites(f, r.Options) {
		var err error
		if _, service := obj.(*corev1.Service); service {
			// Allocated addresses make Services verify-only; their spec is the
			// same for both profiles.
			err = r.ensure(ctx, obj)
		} else {
			err = r.converge(ctx, obj, nil)
		}
		if err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
		}
	}
	if !knownRuntime(runtimeImage(f)) {
		return r.report(ctx, f, h, "UnsupportedTransition", "Provisioning requires a digest-pinned compatible fork runtime; there is no default image", false)
	}
	s := h.j
	if s == nil {
		s = &fleetState{Version: 1, FleetUID: f.UID, Initial: h.res.Spec.InitialReplicas, Claims: map[string]types.UID{}}
	}
	before := stateRendering(s)
	save := func() error {
		if sameState(before, s) && h.j != nil {
			return nil
		}
		return r.saveState(ctx, h.res, s)
	}
	desired := workload(f, r.Options)
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		s.WorkloadUID, s.Applied, s.RuntimeImage, s.RestartToken = desired.GetUID(), replicas(desired), runtimeImage(f), restartToken(f)
		if err := save(); err != nil {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "Provisioning", "Workload created; waiting for runtime readiness and placement", true)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := owned(f, actual); err != nil {
		return r.report(ctx, f, h, "LifecycleBlocked", err.Error(), false)
	}
	s.WorkloadUID, s.Applied = actual.GetUID(), replicas(actual)
	target, automatic := r.capacityTarget(ctx, f, s)
	if target < s.Applied {
		if reason, err := r.bucketContraction(ctx, f, s, actual, automatic); err != nil {
			if err := save(); err != nil {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, &loadedState{res: h.res, j: s}, reason, err.Error(), true)
		}
		// One member per step, so each contraction is observed before the next.
		target = s.Applied - 1
	}
	setReplicas(desired, target)
	if err := r.converge(ctx, desired, actual); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
	}
	s.Applied, s.RuntimeImage, s.RestartToken = target, runtimeImage(f), restartToken(f)
	if err := save(); err != nil {
		return ctrl.Result{}, err
	}
	h = &loadedState{res: h.res, j: s}
	if orderedBucket(f) {
		if err := r.scheduleOrderedBucket(ctx, f, s); err != nil {
			return r.report(ctx, f, h, "SchedulingBlocked", err.Error(), true)
		}
	}
	current := emptyObject(desired)
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return ctrl.Result{}, err
	}
	if !rolledOut(current) {
		return r.report(ctx, f, h, "Provisioning", "Rolling out one member at a time; waiting for updated, ready replicas", true)
	}
	return r.report(ctx, f, h, "Provisioned", "Workload rolled out and all replicas ready", true)
}

// bucketContraction decides whether a one-member contraction may start now.
// A step waits for the previous rollout, and an automatic step also requires
// the victim's load to fit on the survivors: the highest ordinal of a
// StatefulSet, or every Pod of a Deployment, whose victim Kubernetes chooses.
// PersistentFleet callers have already required a settled fleet.
func (r *Reconciler) bucketContraction(ctx context.Context, f *fleet.CelldFleet, s *fleetState, w client.Object, automatic bool) (string, error) {
	if !rolledOut(w) {
		return "Provisioning", errors.New("waiting for the previous change to roll out before removing a member")
	}
	if !automatic && !externalOwner(f) {
		return "", nil
	}
	if r.Collector == nil {
		return "CapacityUncertain", errors.New("survivor metrics unavailable")
	}
	policy := f.DeepCopy()
	if policy.Spec.Capacity == nil {
		policy.Spec.Capacity = &fleet.CapacityPolicy{}
		policy.Spec.Capacity.Default()
	}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return "CapacityUncertain", err
	}
	ids := []string{}
	for i := range pods {
		if id, _ := podIdentity(&pods[i]); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) != int(s.Applied) {
		return "CapacityUncertain", errors.New("membership changed before contraction")
	}
	victims := ids
	if _, ordered := w.(*appsv1.StatefulSet); ordered {
		i := slices.IndexFunc(pods, func(p corev1.Pod) bool { return p.Name == fmt.Sprintf("%s-%d", f.Name, s.Applied-1) })
		if i < 0 {
			return "CapacityUncertain", errors.New("highest ordinal is missing")
		}
		id, _ := podIdentity(&pods[i])
		victims = []string{id}
	}
	observation := r.Collector.Collect(ctx, policy)
	for _, victim := range victims {
		if err := ValidateSurvivors(*policy.Spec.Capacity, observation, ids, victim, r.capacityNow()); err != nil {
			return "CapacityUncertain", err
		}
	}
	return "", nil
}

// rolledOut reports whether the workload controller has observed the current
// spec and every replica is updated and ready.
func rolledOut(w client.Object) bool {
	n := replicas(w)
	switch w := w.(type) {
	case *appsv1.Deployment:
		st := w.Status
		return st.ObservedGeneration >= w.Generation && st.Replicas == n && st.UpdatedReplicas == n && st.ReadyReplicas == n
	case *appsv1.StatefulSet:
		st := w.Status
		return st.ObservedGeneration >= w.Generation && st.Replicas == n && st.UpdatedReplicas == n && st.ReadyReplicas == n && st.CurrentRevision == st.UpdateRevision
	}
	return false
}

// owned refuses to adopt or mutate an object this fleet did not create.
func owned(f *fleet.CelldFleet, obj client.Object) error {
	if obj.GetLabels()[FleetLabel] != string(f.UID) || len(obj.GetOwnerReferences()) != 0 || !obj.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%T %s is not owned by this fleet; refusing adoption or mutation", obj, obj.GetName())
	}
	return nil
}

// converge makes a live Bucket object's operator-owned spec match desired.
// Admission mutates Pods, not these templates, so the whole controlled spec is
// the operator's and drift is corrected rather than blocked. Immutable fields
// (selectors, service name, pod management policy) are rendered identically by
// every release and never rewritten. actual may be nil.
func (r *Reconciler) converge(ctx context.Context, desired, actual client.Object) error {
	if actual == nil {
		actual = emptyObject(desired)
		if err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual); err != nil {
			if apierrors.IsNotFound(err) {
				return r.Create(ctx, desired)
			}
			return err
		}
	}
	if actual.GetLabels()[FleetLabel] != desired.GetLabels()[FleetLabel] || len(actual.GetOwnerReferences()) != 0 || !actual.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%T %s is not owned by this fleet; refusing adoption or mutation", actual, actual.GetName())
	}
	if matches(desired, actual) {
		return nil
	}
	updated := actual.DeepCopyObject().(client.Object)
	switch d := desired.(type) {
	case *appsv1.Deployment:
		u := updated.(*appsv1.Deployment)
		u.Spec.Replicas, u.Spec.Template, u.Spec.Strategy = d.Spec.Replicas, d.Spec.Template, d.Spec.Strategy
	case *appsv1.StatefulSet:
		u := updated.(*appsv1.StatefulSet)
		u.Spec.Replicas, u.Spec.Template, u.Spec.UpdateStrategy, u.Spec.PersistentVolumeClaimRetentionPolicy = d.Spec.Replicas, d.Spec.Template, d.Spec.UpdateStrategy, d.Spec.PersistentVolumeClaimRetentionPolicy
	case *networkingv1.NetworkPolicy:
		updated.(*networkingv1.NetworkPolicy).Spec = d.Spec
	case *policyv1.PodDisruptionBudget:
		updated.(*policyv1.PodDisruptionBudget).Spec = d.Spec
	default:
		return fmt.Errorf("%T %s cannot be converged", actual, actual.GetName())
	}
	// resourceVersion is carried from the read: a concurrent writer yields a
	// Conflict and the next reconcile starts from a fresh read.
	return r.Update(ctx, updated)
}

// deleteBucket removes compute and then releases the finalizer. Members drain
// on SIGTERM. The bucket reservation stays permanent and prerequisites are
// retained, as for every fleet.
func (r *Reconciler) deleteBucket(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	w := emptyObject(workload(f, r.Options))
	err := r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if apierrors.IsNotFound(err) {
		base := f.DeepCopy()
		controllerutil.RemoveFinalizer(f, Finalizer)
		return ctrl.Result{}, r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if w.GetLabels()[FleetLabel] != string(f.UID) || len(w.GetOwnerReferences()) != 0 {
		return r.report(ctx, f, nil, "DeletionBlocked", fmt.Sprintf("%T %s is not owned by this fleet; refusing deletion", w, w.GetName()), false)
	}
	if w.GetDeletionTimestamp().IsZero() {
		if err := r.Delete(ctx, w, client.Preconditions{UID: new(w.GetUID())}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return r.report(ctx, f, nil, "LifecycleProgress", "Deleting workload; members drain on SIGTERM", false)
}

func (r *Reconciler) currentPods(ctx context.Context, f *fleet.CelldFleet, s *fleetState) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f)), client.Limit(101)); err != nil {
		return nil, err
	}
	if list.Continue != "" || len(list.Items) > 100 {
		return nil, errors.New("pod working set exceeds fleet limit")
	}
	for _, p := range list.Items {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.APIVersion != "apps/v1" {
			return nil, errors.New("pod owner missing")
		}
		switch {
		case owner.Kind == "StatefulSet":
			if owner.UID != s.WorkloadUID || owner.Name != f.Name {
				return nil, errors.New("pod workload identity changed")
			}
		case owner.Kind == "ReplicaSet" && f.Spec.Profile == "Bucket" && !orderedBucket(f):
			rs := &appsv1.ReplicaSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: owner.Name}, rs); err != nil {
				return nil, err
			}
			parent := metav1.GetControllerOf(rs)
			if rs.UID != owner.UID || parent == nil || parent.UID != s.WorkloadUID || parent.Name != f.Name || parent.Kind != "Deployment" || !rs.DeletionTimestamp.IsZero() {
				return nil, errors.New("ReplicaSet ownership changed")
			}
		default:
			return nil, errors.New("unexpected pod controller")
		}
	}
	return list.Items, nil
}
