package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/fleethealth"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// PersistentFleet runs CELLD_DURABILITY=fleet on retained CSI disks. celld
// tolerates the loss of any one member, so the operator's only safety job is
// to disrupt one member at a time, wait until celld reports the fleet has
// absorbed it, and never delete an existing disk a session still needs
// (ADR 0023). A disk that is already gone is replaced without waiting.
//
// The StatefulSet keeps OnDelete updates: the operator replaces outdated
// members itself, so an already-down member is updated at once and a running
// one only when the fleet is settled.

const (
	// replaceMemberAnnotation names an ordinal whose existing disk the
	// administrator declares gone. The operator replaces it under the
	// one-disruption rule and then removes the annotation.
	replaceMemberAnnotation = "celld.eric.dev/replace-member"
	// leaseTTL is celld's node lease lifetime (CELLD_TTL_MS). A crash whose
	// lease has not expired still looks like a live leader.
	leaseTTL = 10 * time.Second
	// legacyStabilization gates restarts of runtimes that predate node-log
	// state. Restarts keep disks, so readiness plus this delay suffices; such
	// runtimes never authorize disk deletion.
	legacyStabilization = time.Minute
	revisionLabel       = "controller-revision-hash"
)

// RuntimeStateReader reads celld's node-log state from one Pod.
type RuntimeStateReader interface {
	NodeLog(context.Context, controlplane.Target) (controlplane.NodeLog, error)
}

// member is one expected ordinal as observed this reconcile.
type member struct {
	ordinal int32
	pod     *corev1.Pod
	claim   *corev1.PersistentVolumeClaim
	lost    bool
}

func memberName(f *fleet.CelldFleet, ordinal int32) string {
	return fmt.Sprintf("%s-%d", f.Name, ordinal)
}
func claimName(f *fleet.CelldFleet, ordinal int32) string { return "data-" + memberName(f, ordinal) }

// loadedCurrent discards 0022 operation state no current path uses. Invalid or
// foreign state is dropped rather than blocking; the next save replaces it.
func (r *Reconciler) loadedCurrent(f *fleet.CelldFleet, h *loadedState) *loadedState {
	return r.bucketLoaded(f, h)
}

func (r *Reconciler) reconcilePersistent(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (ctrl.Result, error) {
	for _, obj := range prerequisites(f, r.Options) {
		var err error
		switch obj.(type) {
		case *policyv1.PodDisruptionBudget:
			continue // set below from the fleet's settlement
		case *corev1.Service:
			err = r.ensure(ctx, obj)
		default:
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
	desired := workload(f, r.Options).(*appsv1.StatefulSet)
	actual := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), actual)
	if apierrors.IsNotFound(err) {
		// Claims carrying this fleet's label are its retained disks and are
		// reused; any other claim at a member's name is refused.
		for ordinal := range f.Spec.Replicas {
			claim := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: claimName(f, ordinal)}, claim); err == nil && claim.Labels[FleetLabel] != string(f.UID) {
				return r.report(ctx, f, h, "StorageIdentityConflict", fmt.Sprintf("PVC %s exists and does not belong to this fleet; refusing to adopt it", claim.Name), false)
			} else if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		s.WorkloadUID, s.Applied, s.RuntimeImage, s.RestartToken = desired.UID, replicas(desired), runtimeImage(f), restartToken(f)
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
	s.WorkloadUID, s.Applied = actual.UID, replicas(actual)
	members, orphans, err := r.persistentMembers(ctx, f, actual)
	if err != nil {
		return r.report(ctx, f, h, "LifecycleBlocked", err.Error(), false)
	}
	now := r.capacityNow()
	a := r.assessPersistent(ctx, f, s, members)
	settled := a.Settled
	if err := r.converge(ctx, persistentBudget(f, settled), nil); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
	}
	disrupt := func(what string) (ctrl.Result, error) {
		s.LastDisruption = now
		// Load samples cannot span a disruption.
		resetMaintenanceCapacity(s)
		if err := save(); err != nil {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, &loadedState{res: h.res, j: s}, "LifecycleProgress", what, true)
	}

	// A disk that is gone is gone. Replace its member at once; celld
	// recovers each dependent session from another copy or records a loss.
	for _, m := range members {
		if !m.lost {
			continue
		}
		msg := fmt.Sprintf("Replacing member %s: its disk no longer exists", memberName(f, m.ordinal))
		if sessions, ok := a.Obligations(memberName(f, m.ordinal)); ok && len(sessions) > 0 {
			msg += "; celld records a loss for any of these sessions without another copy: " + strings.Join(sessions, ", ")
		}
		if err := r.replaceMember(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
		r.eventf(f, corev1.EventTypeWarning, "MemberDiskLost", "%s", msg)
		return disrupt(msg)
	}

	// Removed members' disks are deleted only once no session needs them.
	if len(orphans) > 0 {
		claim := orphans[0]
		node := strings.TrimPrefix(claim.Name, "data-")
		if !a.Releasable(node) {
			sessions, ok := a.Obligations(node)
			why := "fleet node-log state unknown"
			if ok {
				why = "still needed by " + strings.Join(sessions, ", ")
			}
			return r.report(ctx, f, h, "LifecycleProgress", fmt.Sprintf("Retaining disk %s of removed member %s: %s", claim.Name, node, why), true)
		}
		if err := r.Delete(ctx, claim, client.Preconditions{UID: new(claim.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "LifecycleProgress", fmt.Sprintf("Deleted disk %s of removed member %s", claim.Name, node), true)
	}

	// An administrator-declared replacement runs under the one-disruption
	// rule; the member's own absence does not count against it.
	if raw := f.Annotations[replaceMemberAnnotation]; raw != "" {
		m, err := replacementTarget(f, raw, members)
		if err != nil {
			return r.report(ctx, f, h, "ReplaceMemberInvalid", err.Error(), true)
		}
		if !settled && !onlyDisrupted(members, m) {
			return r.report(ctx, f, h, "LifecycleProgress", "Replacement of "+memberName(f, m.ordinal)+" waits for the fleet to settle: "+a.Reason, true)
		}
		msg := fmt.Sprintf("Replacing member %s on administrator request", memberName(f, m.ordinal))
		if sessions, ok := a.Obligations(memberName(f, m.ordinal)); ok && len(sessions) > 0 {
			msg += "; celld records a loss for any of these sessions without another copy: " + strings.Join(sessions, ", ")
		}
		if err := r.replaceMember(ctx, m); err != nil {
			return ctrl.Result{}, err
		}
		base := f.DeepCopy()
		delete(f.Annotations, replaceMemberAnnotation)
		if err := r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		r.eventf(f, corev1.EventTypeWarning, "MemberReplaced", "%s", msg)
		return disrupt(msg)
	}

	// Template changes (runtime image, restart token, a new release) are
	// written first; OnDelete keeps every running member as it is.
	desired.Spec.Replicas = new(s.Applied)
	if err := r.converge(ctx, desired, actual); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "InfrastructureBlocked", err.Error(), false)
	}
	s.RuntimeImage, s.RestartToken = runtimeImage(f), restartToken(f)
	if err := r.releaseLauncherGates(ctx, members); err != nil {
		return r.report(ctx, f, h, "SchedulingBlocked", err.Error(), true)
	}
	current := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		return ctrl.Result{}, err
	}
	if current.Status.ObservedGeneration < current.Generation || current.Status.UpdateRevision == "" {
		if err := save(); err != nil {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, h, "Provisioning", "Waiting for the StatefulSet controller to observe the template", true)
	}
	if outdated := outdatedMembers(members, current.Status.UpdateRevision); len(outdated) > 0 {
		// A member that is already down is updated at once: replacing it adds
		// no disruption. A running member waits for the fleet to settle.
		m := outdated[0]
		if podReady(m.pod) && !a.rollable(members, s, now) {
			if err := save(); err != nil {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, h, "LifecycleProgress", fmt.Sprintf("Rolling update waits before %s: %s", m.pod.Name, a.Reason), true)
		}
		if err := r.Delete(ctx, m.pod, client.Preconditions{UID: new(m.pod.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return disrupt(fmt.Sprintf("Rolling update replaced %s; %d outdated member(s) remain", m.pod.Name, len(outdated)-1))
	}

	target, automatic := r.capacityTarget(ctx, f, s)
	switch {
	case target > s.Applied:
		// Growth reuses no removed member's disk.
		if len(orphans) > 0 {
			return r.report(ctx, f, h, "LifecycleProgress", "Growth waits for removal of retained disk "+orphans[0].Name, true)
		}
		current.Spec.Replicas = new(target)
		if err := r.Update(ctx, current); err != nil {
			return ctrl.Result{}, err
		}
		s.Applied = target
		if err := save(); err != nil {
			return ctrl.Result{}, err
		}
		return r.report(ctx, f, &loadedState{res: h.res, j: s}, "Provisioning", fmt.Sprintf("Adding members up to %d", target), true)
	case target < s.Applied:
		if !settled {
			if err := save(); err != nil {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, &loadedState{res: h.res, j: s}, "LifecycleProgress", "Contraction waits for the fleet to settle: "+a.Reason, true)
		}
		if reason, err := r.bucketContraction(ctx, f, s, current, automatic); err != nil {
			if err := save(); err != nil {
				return ctrl.Result{}, err
			}
			return r.report(ctx, f, &loadedState{res: h.res, j: s}, reason, err.Error(), true)
		}
		current.Spec.Replicas = new(s.Applied - 1)
		if err := r.Update(ctx, current); err != nil {
			return ctrl.Result{}, err
		}
		s.Applied--
		return disrupt(fmt.Sprintf("Removing member %s; its disk is deleted once no session needs it", memberName(f, s.Applied)))
	}
	if err := save(); err != nil {
		return ctrl.Result{}, err
	}
	h = &loadedState{res: h.res, j: s}
	if !rolledOut(current) {
		return r.report(ctx, f, h, "Provisioning", "Waiting for updated, ready replicas", true)
	}
	if !settled {
		return r.report(ctx, f, h, "LifecycleProgress", "Fleet is recovering: "+a.Reason, true)
	}
	return r.report(ctx, f, h, "Provisioned", "All members ready and the fleet is settled", true)
}

// persistentMembers maps expected ordinals to their Pods and claims, marks
// members whose disk no longer exists, and returns claims of removed members.
func (r *Reconciler) persistentMembers(ctx context.Context, f *fleet.CelldFleet, sts *appsv1.StatefulSet) ([]member, []*corev1.PersistentVolumeClaim, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return nil, nil, err
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return nil, nil, err
	}
	n := replicas(sts)
	members := make([]member, n)
	for i := range members {
		members[i].ordinal = int32(i)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.UID != sts.UID {
			return nil, nil, fmt.Errorf("pod %s is not owned by this fleet's StatefulSet", p.Name)
		}
		if o, err := bucketOrdinal(f, p.Name); err == nil && o < int(n) {
			members[o].pod = p
		}
	}
	var orphans []*corev1.PersistentVolumeClaim
	for i := range claims.Items {
		c := &claims.Items[i]
		o, err := bucketOrdinal(f, strings.TrimPrefix(c.Name, "data-"))
		if err != nil || !strings.HasPrefix(c.Name, "data-") {
			continue
		}
		if o >= int(n) {
			if c.DeletionTimestamp.IsZero() {
				orphans = append(orphans, c)
			}
			continue
		}
		members[o].claim = c
	}
	slices.SortFunc(orphans, func(a, b *corev1.PersistentVolumeClaim) int { return strings.Compare(b.Name, a.Name) })
	for i := range members {
		lost, err := r.diskLost(ctx, &members[i])
		if err != nil {
			return nil, nil, err
		}
		members[i].lost = lost
	}
	return members, orphans, nil
}

// diskLost reports a member whose disk no longer exists: its claim is Lost,
// its bound volume is gone, or its Pod waits for a claim that was deleted.
func (r *Reconciler) diskLost(ctx context.Context, m *member) (bool, error) {
	c := m.claim
	if c == nil {
		return m.pod != nil && m.pod.Spec.NodeName == "" && m.pod.DeletionTimestamp.IsZero() && m.pod.Status.Phase == corev1.PodPending && !podRecent(m.pod), nil
	}
	if !c.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if c.Status.Phase == corev1.ClaimLost {
		return true, nil
	}
	if c.Spec.VolumeName == "" {
		return false, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: c.Spec.VolumeName}, pv); apierrors.IsNotFound(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// podRecent leaves a just-created Pod time for the StatefulSet controller to
// create its claim before a missing claim is treated as a lost disk.
func podRecent(p *corev1.Pod) bool { return time.Since(p.CreationTimestamp.Time) < time.Minute }

func (r *Reconciler) replaceMember(ctx context.Context, m member) error {
	if c := m.claim; c != nil && c.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, c, client.Preconditions{UID: new(c.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if p := m.pod; p != nil && p.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, p, client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func replacementTarget(f *fleet.CelldFleet, raw string, members []member) (member, error) {
	for _, m := range members {
		if raw == fmt.Sprint(m.ordinal) || raw == memberName(f, m.ordinal) {
			return m, nil
		}
	}
	return member{}, fmt.Errorf("%s=%q names no current member", replaceMemberAnnotation, raw)
}

// onlyDisrupted reports whether target is down and every other member is
// ready, so replacing it adds no disruption.
func onlyDisrupted(members []member, target member) bool {
	if target.pod != nil && podReady(target.pod) && target.pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, m := range members {
		if m.ordinal != target.ordinal && (m.pod == nil || !podReady(m.pod) || !m.pod.DeletionTimestamp.IsZero()) {
			return false
		}
	}
	return true
}

// outdatedMembers lists Pods not at the update revision, members already down
// first, then highest ordinal first.
func outdatedMembers(members []member, revision string) []member {
	var out []member
	for _, m := range members {
		if m.pod != nil && m.pod.DeletionTimestamp.IsZero() && m.pod.Labels[revisionLabel] != revision {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b member) int {
		if ra, rb := podReady(a.pod), podReady(b.pod); ra != rb {
			if ra {
				return 1
			}
			return -1
		}
		return int(b.ordinal - a.ordinal)
	})
	return out
}

// releaseLauncherGates admits Pods created from a template written by an
// earlier release, whose launcher gate no current path releases otherwise.
func (r *Reconciler) releaseLauncherGates(ctx context.Context, members []member) error {
	for _, m := range members {
		p := m.pod
		if p == nil || p.Spec.NodeName != "" || !slices.ContainsFunc(p.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == launcherGate }) {
			continue
		}
		p = p.DeepCopy()
		p.Spec.SchedulingGates = slices.DeleteFunc(p.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == launcherGate })
		if err := r.Update(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// persistentAssessment adds the operator's own facts to celld's assessment.
type persistentAssessment struct {
	fleethealth.Assessment
	legacy  bool
	horizon time.Time
}

// assessPersistent reads every expected member's node-log state. The horizon
// is the later of the last operator disruption and any member's latest
// (re)start, plus one lease TTL.
func (r *Reconciler) assessPersistent(ctx context.Context, f *fleet.CelldFleet, s *fleetState, members []member) persistentAssessment {
	last := s.LastDisruption
	observed := make([]fleethealth.Member, 0, len(members))
	legacy := false
	for _, m := range members {
		o := fleethealth.Member{Node: memberName(f, m.ordinal)}
		if p := m.pod; p != nil {
			if p.CreationTimestamp.After(last) {
				last = p.CreationTimestamp.Time
			}
			for _, c := range p.Status.ContainerStatuses {
				if c.State.Running != nil && c.State.Running.StartedAt.After(last) {
					last = c.State.Running.StartedAt.Time
				}
			}
			o.Ready = podReady(p) && p.DeletionTimestamp.IsZero()
		}
		if o.Ready {
			log, err := r.nodeLog(ctx, f, m.pod)
			o.Log, o.Err = log, err
			legacy = legacy || errors.Is(err, controlplane.ErrNoNodeLog)
		}
		observed = append(observed, o)
	}
	horizon := last.Add(leaseTTL)
	return persistentAssessment{Assessment: fleethealth.Assess(observed, horizon), legacy: legacy, horizon: last}
}

// rollable reports whether a running member may be restarted now. A restart
// keeps the member's disk, so a runtime that predates node-log state may roll
// on readiness plus a stabilization delay; it can never release a disk.
func (a persistentAssessment) rollable(members []member, _ *fleetState, now time.Time) bool {
	if a.Settled {
		return true
	}
	if !a.legacy || now.Before(a.horizon.Add(legacyStabilization)) {
		return false
	}
	for _, m := range members {
		if m.pod == nil || !podReady(m.pod) || !m.pod.DeletionTimestamp.IsZero() {
			return false
		}
	}
	return true
}

func (r *Reconciler) nodeLog(ctx context.Context, f *fleet.CelldFleet, p *corev1.Pod) (*controlplane.NodeLog, error) {
	if r.RuntimeState == nil {
		return nil, errors.New("runtime state reader unavailable")
	}
	target, err := runtimeTarget(f, p, "")
	if err != nil {
		return nil, err
	}
	log, err := r.RuntimeState.NodeLog(ctx, target)
	if err != nil {
		return nil, err
	}
	return &log, nil
}

// persistentBudget lets one voluntary eviction proceed only while the fleet
// is settled, so node drains wait out recovery.
func persistentBudget(f *fleet.CelldFleet, settled bool) *policyv1.PodDisruptionBudget {
	for _, obj := range prerequisites(f, Options{}) {
		if pdb, ok := obj.(*policyv1.PodDisruptionBudget); ok {
			if !settled {
				pdb.Spec.MaxUnavailable = new(intstr.FromInt32(0))
			}
			return pdb
		}
	}
	panic("prerequisites render no PodDisruptionBudget")
}

func (r *Reconciler) eventf(f *fleet.CelldFleet, kind, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(f, nil, kind, reason, "Reconcile", format, args...)
	}
}

// deletePersistent removes compute, then every disk, then the finalizer.
// Members drain on SIGTERM. The bucket reservation stays permanent.
func (r *Reconciler) deletePersistent(ctx context.Context, f *fleet.CelldFleet) (ctrl.Result, error) {
	w := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKeyFromObject(f), w)
	if err == nil {
		if w.Labels[FleetLabel] != string(f.UID) || len(w.OwnerReferences) != 0 {
			return r.report(ctx, f, nil, "DeletionBlocked", fmt.Sprintf("StatefulSet %s is not owned by this fleet; refusing deletion", w.Name), false)
		}
		if w.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, w, client.Preconditions{UID: new(w.UID)}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return r.report(ctx, f, nil, "LifecycleProgress", "Deleting workload; members drain on SIGTERM", false)
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return ctrl.Result{}, err
	}
	if len(claims.Items) > 0 {
		for i := range claims.Items {
			c := &claims.Items[i]
			if c.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, c, client.Preconditions{UID: new(c.UID)}); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
		}
		return r.report(ctx, f, nil, "LifecycleProgress", "Deleting disks", false)
	}
	base := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	return ctrl.Result{}, r.Patch(ctx, f, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
