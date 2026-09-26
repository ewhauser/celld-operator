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

// Both profiles are ordinary Kubernetes workloads. celld hands cells off on
// SIGTERM, recovers a restarted node from its own disk and its peers, and
// holds a new node's first healthy response until the fleet has absorbed the
// change, so the workload controller's rolling update is the whole lifecycle
// (ADR 0023, ADR 0024). The operator renders the desired objects, applies
// them, steps contraction one member at a time, and replaces a StatefulSet
// member that cannot come back on its own (persistent.go). It persists
// nothing but capacity-policy state, once a fleet sets spec.capacity.
//
// Bucket fleets run CELLD_DURABILITY=bucket: their disks are caches.
// PersistentFleet runs CELLD_DURABILITY=fleet on a StatefulSet whose members
// keep their claims for the life of the fleet (persistent.go).

// currentState returns the persisted capacity history. Invalid or foreign
// state is dropped rather than blocking: nothing in it can make a workload
// change unsafe, and the next save replaces it.
func (r *Reconciler) currentState(f *fleet.CelldFleet, h *loadedState) *loadedState {
	out := &loadedState{res: h.res, j: h.j}
	if h.err != nil || (h.j != nil && h.j.FleetUID != f.UID) {
		out.j = nil
	}
	return out
}

func (r *Reconciler) reconcileWorkload(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (ctrl.Result, error) {
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
		s = &fleetState{Version: 1, FleetUID: f.UID}
	}
	desired := workload(f, r.Options)
	actual := emptyObject(desired)
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		if conflict, err := r.foreignClaim(ctx, f, replicas(desired)); err != nil {
			return ctrl.Result{}, err
		} else if conflict != "" {
			return r.report(ctx, f, h, "StorageIdentityConflict", conflict, false)
		}
		if err := r.Create(ctx, desired); err != nil {
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
	applied := replicas(actual)
	target, automatic := r.capacityTarget(ctx, f, s, applied)
	var waitReason, waitMessage string
	blocked := false
	switch {
	case target < applied:
		if reason, err := r.contraction(ctx, f, desired, actual, automatic); err != nil {
			// Template changes still apply; only the removal waits.
			target, waitReason, waitMessage = applied, reason, err.Error()
		} else {
			// One member per step, so each contraction is observed before the next.
			target = applied - 1
		}
	case target > applied:
		if conflict, err := r.foreignClaim(ctx, f, target); err != nil {
			return ctrl.Result{}, err
		} else if conflict != "" {
			target, waitReason, waitMessage, blocked = applied, "StorageIdentityConflict", conflict, true
		}
	}
	setReplicas(desired, target)
	if err := r.converge(ctx, desired, actual); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
	}
	if err := r.saveState(ctx, h.res, s); err != nil {
		return ctrl.Result{}, err
	}
	if op := s.Operation; op != nil {
		// The save above dropped a strict operation an earlier release recorded.
		r.eventf(f, corev1.EventTypeNormal, "OperationSuperseded", "Dropped %s operation %s recorded by an earlier release; members roll onto the current template instead", op.Kind, op.ID)
		s.Operation = nil
	}
	h = &loadedState{res: h.res, j: s}
	waiting := ""
	if sts, ok := actual.(*appsv1.StatefulSet); ok {
		action, note, err := r.healMembers(ctx, f, sts)
		if err != nil {
			return ctrl.Result{}, err
		}
		if action != "" {
			return r.report(ctx, f, h, "LifecycleProgress", action, true)
		}
		waiting = note
	}
	// Ordered Bucket Pods wait for a zone before scheduling, including those a
	// rollout creates while a removal waits for it.
	if orderedBucket(f) {
		if err := r.scheduleOrderedBucket(ctx, f, actual.GetUID()); err != nil {
			return r.report(ctx, f, h, "SchedulingBlocked", err.Error(), true)
		}
	}
	if waitReason != "" {
		return r.report(ctx, f, h, waitReason, waitMessage, !blocked)
	}
	current := emptyObject(desired)
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return ctrl.Result{}, err
	}
	if waiting != "" {
		return r.report(ctx, f, h, "Provisioning", "Waiting for ready replicas; "+waiting, true)
	}
	if !rolledOut(current) {
		return r.report(ctx, f, h, "Provisioning", "Rolling out one member at a time; waiting for updated, ready replicas", true)
	}
	return r.report(ctx, f, h, "Provisioned", "Workload rolled out and all replicas ready", true)
}

// contraction decides whether a one-member contraction may start now. A step
// waits until the workload runs the desired template and has rolled out, so a
// removal never overlaps a restart. An automatic step also requires the
// victim's load to fit on the survivors: the highest ordinal of a
// StatefulSet, or every Pod of a Deployment, whose victim Kubernetes chooses.
func (r *Reconciler) contraction(ctx context.Context, f *fleet.CelldFleet, desired, actual client.Object, automatic bool) (string, error) {
	current := desired.DeepCopyObject().(client.Object)
	setReplicas(current, replicas(actual))
	if !matches(current, actual) || !rolledOut(actual) {
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
	pods, err := r.currentPods(ctx, f, actual.GetUID())
	if err != nil {
		return "CapacityUncertain", err
	}
	ids := []string{}
	for i := range pods {
		if id, _ := podIdentity(&pods[i]); id != "" {
			ids = append(ids, id)
		}
	}
	applied := replicas(actual)
	if len(ids) != int(applied) {
		return "CapacityUncertain", errors.New("membership changed before contraction")
	}
	victims := ids
	if _, ordered := actual.(*appsv1.StatefulSet); ordered {
		i := slices.IndexFunc(pods, func(p corev1.Pod) bool { return p.Name == fmt.Sprintf("%s-%d", f.Name, applied-1) })
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
		done := st.ObservedGeneration >= w.Generation && st.Replicas == n && st.UpdatedReplicas == n && st.ReadyReplicas == n
		// The StatefulSet controller advances currentRevision only for
		// RollingUpdate. A StatefulSet written by an earlier release keeps
		// OnDelete until the operator converges it, and under OnDelete
		// currentRevision stays at the creation revision forever.
		if w.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			return done
		}
		return done && st.CurrentRevision == st.UpdateRevision
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

// converge makes a live object's operator-owned spec match desired.
// Admission mutates Pods, not these templates, so the whole controlled spec is
// the operator's and drift is corrected rather than blocked. Immutable fields
// (selectors, service name, pod management policy, claim templates) are
// rendered identically by every release and never rewritten. actual may be nil.
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

// deleteWorkload removes compute, then, for PersistentFleet, every disk, and
// then releases the finalizer. Members drain on SIGTERM. The bucket
// reservation stays permanent and prerequisites are retained, as for every
// fleet.
func (r *Reconciler) deleteWorkload(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	w := emptyObject(workload(f, r.Options))
	err := r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if err == nil {
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
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if remaining, err := r.deleteClaims(ctx, f); err != nil {
		return ctrl.Result{}, err
	} else if remaining {
		return r.report(ctx, f, nil, "LifecycleProgress", "Deleting disks", false)
	}
	base := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	return ctrl.Result{}, r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// currentPods lists the fleet's Pods and requires each to belong to the
// workload with the given UID.
func (r *Reconciler) currentPods(ctx context.Context, f *fleet.CelldFleet, workloadUID types.UID) ([]corev1.Pod, error) {
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
			if owner.UID != workloadUID || owner.Name != f.Name {
				return nil, errors.New("pod workload identity changed")
			}
		case owner.Kind == "ReplicaSet" && f.Spec.Profile == "Bucket" && !orderedBucket(f):
			rs := &appsv1.ReplicaSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: owner.Name}, rs); err != nil {
				return nil, err
			}
			parent := metav1.GetControllerOf(rs)
			if rs.UID != owner.UID || parent == nil || parent.UID != workloadUID || parent.Name != f.Name || parent.Kind != "Deployment" || !rs.DeletionTimestamp.IsZero() {
				return nil, errors.New("ReplicaSet ownership changed")
			}
		default:
			return nil, errors.New("unexpected pod controller")
		}
	}
	return list.Items, nil
}
