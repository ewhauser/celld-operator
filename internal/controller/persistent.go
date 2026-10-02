package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PersistentFleet runs CELLD_DURABILITY=fleet on a StatefulSet. Each member
// keeps its claim for the life of the fleet: restart, upgrade and scale-in
// retain it, and scaling back out reattaches it, which celld treats as a
// restart on the same disk. The StatefulSet controller restarts one member at
// a time and celld recovers each one (ADR 0024).
//
// The fleet heals itself; no administrator is expected to intervene. A member
// that cannot come back on its own is replaced, judged from what the cluster
// reports on each reconcile, and nothing about it is recorded:
//
//   - A member Pod still present well after its termination grace is on a
//     node that no longer answers. It is force-deleted so the StatefulSet can
//     recreate it, and its disk is kept. celld fences any process that
//     survives on that node through its lease.
//   - A claim that Kubernetes marks Lost has no volume behind it. The member's
//     claim and Pod are deleted, and the StatefulSet recreates both.
//   - A member that has stayed down for the replacement delay, while every
//     other member has been ready for absorbWindow, is treated as lost. By then
//     celld has moved every session off it, so its claim and Pod are deleted
//     and it returns on a fresh disk. A member waiting on its image or
//     configuration, or already on a disk made after its Pod, is left alone:
//     a new disk would not help it.
//
// A zonal disk pins its member to one zone, and the zone spread counts only
// Pods that are already on nodes. A member whose Pod is pending leaves its
// zone looking free, so a fresh disk scheduled beside it can take that zone
// and leave it nowhere to run (#79). A fresh disk is therefore given only
// once the StatefulSet has caught up with the operator's spec, so a rollout
// cannot recreate another member alongside it, and the scheduler has placed,
// or found no node for, every other member's Pod. The fresh disk then goes to
// the zone the fleet is missing. Growth follows the same rule, one run of kept
// or fresh disks at a time (growth).
//
// Each reconcile takes at most one of these actions. A member the scheduler
// cannot place is named in the fleet's status, and once it has waited out the
// replacement delay without being replaced, the fleet is Blocked on it.

const (
	// DefaultMemberReplacementDelay is how long a member may stay down, while
	// the rest of the fleet is ready, before it is replaced on a fresh disk.
	// It is patience, not safety: absorbWindow is what makes the old disk
	// unneeded. Ten minutes outlasts the six Kubernetes takes to force-detach
	// a volume from a lost node, and typical node provisioning, so a member
	// that can return usually does so on its own disk.
	DefaultMemberReplacementDelay = 10 * time.Minute
	// terminationMargin is how long past its termination grace a deleted Pod
	// may remain before its node is presumed unreachable.
	terminationMargin = 2 * time.Minute
	// absorbWindow is how long every other member must have been ready before
	// a down member's disk is released. Leaders drop a departed follower
	// within seconds, and every node runs the dead-leader sweep every 30
	// seconds.
	absorbWindow = 5 * time.Minute
)

// configWaits are container waiting reasons that no disk replacement fixes.
var configWaits = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError"}

func memberName(f *fleet.CelldFleet, ordinal int32) string {
	return fmt.Sprintf("%s-%d", f.Name, ordinal)
}
func claimName(f *fleet.CelldFleet, ordinal int32) string { return "data-" + memberName(f, ordinal) }

func (r *Reconciler) memberReplacementDelay() time.Duration {
	if r.Options.MemberReplacementDelay > 0 {
		return r.Options.MemberReplacementDelay
	}
	return DefaultMemberReplacementDelay
}

// healMembers takes at most one self-healing action and describes it. When no
// action is due, note describes a down member: when it will be replaced, or
// why the scheduler cannot place it. blocked reports a member the scheduler
// has not placed for the replacement delay and that is not being replaced.
// settled reports that the StatefulSet already runs the operator's spec and
// has observed it.
func (r *Reconciler) healMembers(ctx context.Context, f *fleet.CelldFleet, sts *appsv1.StatefulSet, settled bool) (action, note string, blocked bool, err error) {
	pods, err := r.memberPods(ctx, f, sts)
	if err != nil {
		return "", "", false, err
	}
	now := r.capacityNow()
	for i := range pods {
		p := &pods[i]
		if !p.DeletionTimestamp.IsZero() && now.Sub(p.DeletionTimestamp.Time) > terminationMargin {
			if err := r.Delete(ctx, p, client.GracePeriodSeconds(0), client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
				return "", "", false, err
			}
			return r.healed(f, "MemberForceDeleted", fmt.Sprintf("Force-deleted Pod %s: its node has not confirmed termination; the member returns on its own disk", p.Name))
		}
	}
	if f.Spec.Profile != "PersistentFleet" {
		return "", "", false, nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return "", "", false, err
	}
	podOf := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		podOf[pods[i].Name] = &pods[i]
	}
	claimOf := make(map[string]*corev1.PersistentVolumeClaim, len(claims.Items))
	for i := range claims.Items {
		claimOf[claims.Items[i].Name] = &claims.Items[i]
	}
	// fresh reports whether a member may be given a fresh disk now: no rollout
	// is about to recreate another member, and the scheduler has decided every
	// other member's Pod, so each disk that pins a member to a zone is counted.
	fresh := func(ordinal int32) bool {
		if !settled {
			return false
		}
		for other := range replicas(sts) {
			if other != ordinal && !scheduleDecided(podOf[memberName(f, other)]) {
				return false
			}
		}
		return true
	}
	var down []int32
	for ordinal := range replicas(sts) {
		name := memberName(f, ordinal)
		p, c := podOf[name], claimOf[claimName(f, ordinal)]
		switch {
		case c != nil && !c.DeletionTimestamp.IsZero():
			// A replacement in progress: a scheduled Pod holds the claim until
			// it is gone. An unscheduled one does not.
			if p != nil && p.DeletionTimestamp.IsZero() && p.Spec.NodeName != "" && fresh(ordinal) {
				if err := r.Delete(ctx, p, client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
					return "", "", false, err
				}
				return fmt.Sprintf("Replacing member %s: deleting its Pod so its old disk can be released", name), "", false, nil
			}
		case c != nil && c.Status.Phase == corev1.ClaimLost && fresh(ordinal):
			if err := r.replaceMember(ctx, c, p); err != nil {
				return "", "", false, err
			}
			return r.healed(f, "MemberDiskLost", fmt.Sprintf("Replacing member %s: its volume no longer exists; celld records a bounded loss for any session with no other copy", name))
		}
		if p == nil || !p.DeletionTimestamp.IsZero() || !podReady(p) {
			down = append(down, ordinal)
		}
	}
	if len(down) == 1 {
		name := memberName(f, down[0])
		p, c := podOf[name], claimOf[claimName(f, down[0])]
		if due, ok := r.replacementDue(f, sts, podOf, c, down[0], now); ok {
			if now.Before(due) {
				return "", fmt.Sprintf("%s; it is replaced on a fresh disk at %s unless it returns", describeDown(name, p), due.UTC().Format(time.RFC3339)), false, nil
			}
			if !fresh(down[0]) {
				return "", describeDown(name, p) + "; it is due to be replaced on a fresh disk", false, nil
			}
			if err := r.replaceMember(ctx, c, p); err != nil {
				return "", "", false, err
			}
			return r.healed(f, "MemberReplaced", fmt.Sprintf("Replacing member %s: down since %s while the rest of the fleet is ready; it returns on a fresh disk", name, notReadySince(p).UTC().Format(time.RFC3339)))
		}
	}
	// No replacement is planned. A member the scheduler cannot place is named,
	// and once it has waited out the replacement delay the fleet is blocked on
	// it.
	for _, ordinal := range down {
		name := memberName(f, ordinal)
		p := podOf[name]
		s := unschedulable(p)
		if s == nil {
			continue
		}
		since := s.LastTransitionTime.Time
		if since.IsZero() {
			since = p.CreationTimestamp.Time
		}
		if now.Sub(since) < r.memberReplacementDelay() {
			return "", describeDown(name, p), false, nil
		}
		message := fmt.Sprintf("Member %s has not been scheduled since %s: %s", name, since.UTC().Format(time.RFC3339), schedulerReason(s))
		if i := slices.IndexFunc(down, func(other int32) bool { return other != ordinal }); i >= 0 {
			message += fmt.Sprintf("; it is not replaced while member %s is also down", memberName(f, down[i]))
		}
		return "", message, true, nil
	}
	return "", "", false, nil
}

// memberPods lists the StatefulSet's Pods. Only Pods it controls are
// members; anything else that carries the fleet's label is ignored.
func (r *Reconciler) memberPods(ctx context.Context, f *fleet.CelldFleet, sts *appsv1.StatefulSet) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return nil, err
	}
	// List owns this working set. Compact it instead of allocating and growing
	// a second slice of large Pod structs on every reconcile.
	pods := list.Items[:0]
	for i := range list.Items {
		if owner := metav1.GetControllerOf(&list.Items[i]); owner != nil && owner.UID == sts.UID {
			pods = append(pods, list.Items[i])
		}
	}
	clear(list.Items[len(pods):])
	return pods, nil
}

// growth returns how far a PersistentFleet grows toward target now, or why it
// does not grow yet. The StatefulSet creates every new Pod at once, and a Pod
// that reattaches a kept disk is pinned to that disk's zone while a Pod on a
// fresh disk may take any zone the spread allows. Created together, the fresh
// disk can take the zone a kept disk needs (#79). Growth therefore adds one
// run of ordinals at a time, all of them reattaching kept claims or all of
// them getting fresh disks, and each run waits until the scheduler has
// decided every current member's Pod, so the zones kept disks hold are
// counted.
func (r *Reconciler) growth(ctx context.Context, f *fleet.CelldFleet, sts *appsv1.StatefulSet, target int32) (int32, string, error) {
	applied := replicas(sts)
	if f.Spec.Profile != "PersistentFleet" {
		return target, "", nil
	}
	pods, err := r.memberPods(ctx, f, sts)
	if err != nil {
		return 0, "", err
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return 0, "", err
	}
	kept := make(map[string]bool, len(claims.Items))
	for i := range claims.Items {
		c := &claims.Items[i]
		kept[c.Name] = c.DeletionTimestamp.IsZero()
	}
	end := applied + 1
	for end < target && kept[claimName(f, end)] == kept[claimName(f, applied)] {
		end++
	}
	adding := fmt.Sprintf("adding member %s on a fresh disk", memberName(f, applied))
	switch {
	case kept[claimName(f, applied)] && end-applied > 1:
		adding = fmt.Sprintf("adding members %s to %s on their kept disks", memberName(f, applied), memberName(f, end-1))
	case kept[claimName(f, applied)]:
		adding = fmt.Sprintf("adding member %s on its kept disk", memberName(f, applied))
	case end-applied > 1:
		adding = fmt.Sprintf("adding members %s to %s on fresh disks", memberName(f, applied), memberName(f, end-1))
	}
	if !observed(sts) {
		return applied, "Waiting for the StatefulSet to observe its spec before " + adding, nil
	}
	podOf := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		podOf[pods[i].Name] = &pods[i]
	}
	for ordinal := range applied {
		if name := memberName(f, ordinal); !scheduleDecided(podOf[name]) {
			return applied, fmt.Sprintf("Waiting for the scheduler to place member %s, or find no node for it, before %s", name, adding), nil
		}
	}
	return end, "", nil
}

func (r *Reconciler) healed(f *fleet.CelldFleet, reason, message string) (string, string, bool, error) {
	r.eventf(f, corev1.EventTypeWarning, reason, "%s", message)
	return message, "", false, nil
}

// replacementDue reports when the one member that is down is replaced on a
// fresh disk, if it is replaced at all.
func (r *Reconciler) replacementDue(f *fleet.CelldFleet, sts *appsv1.StatefulSet, podOf map[string]*corev1.Pod, c *corev1.PersistentVolumeClaim, ordinal int32, now time.Time) (time.Time, bool) {
	p := podOf[memberName(f, ordinal)]
	if p == nil || !p.DeletionTimestamp.IsZero() || c == nil || c.Status.Phase != corev1.ClaimBound || waitingOnConfig(p) {
		return time.Time{}, false
	}
	// The StatefulSet creates a claim just before or after the Pod that needs
	// it. A disk that new was made for this Pod; replacing it again would not
	// help.
	if !c.CreationTimestamp.Time.Before(p.CreationTimestamp.Add(-time.Minute)) {
		return time.Time{}, false
	}
	for other := range replicas(sts) {
		if other == ordinal {
			continue
		}
		if since, ok := readySince(podOf[memberName(f, other)]); !ok || now.Sub(since) < absorbWindow {
			return time.Time{}, false
		}
	}
	return notReadySince(p).Add(r.memberReplacementDelay()), true
}

// describeDown names a down member and, when the scheduler finds no node for
// it, gives the scheduler's reason.
func describeDown(name string, p *corev1.Pod) string {
	if s := unschedulable(p); s != nil {
		return fmt.Sprintf("member %s cannot be scheduled (%s)", name, schedulerReason(s))
	}
	return fmt.Sprintf("member %s is down", name)
}

// unschedulable returns the scheduler's verdict on a member Pod it has found
// no node for, or nil.
func unschedulable(p *corev1.Pod) *corev1.PodCondition {
	if p == nil || !p.DeletionTimestamp.IsZero() || p.Spec.NodeName != "" {
		return nil
	}
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c
		}
	}
	return nil
}

// schedulerReason is the scheduler's explanation, to be quoted in a sentence.
func schedulerReason(s *corev1.PodCondition) string {
	return strings.TrimRight(s.Message, ". ")
}

// scheduleDecided reports whether the scheduler has decided a member Pod: it
// is on a node, or the scheduler has found no node for it. A Pod that is
// being recreated, or that the scheduler has not tried yet, is undecided.
func scheduleDecided(p *corev1.Pod) bool {
	return p != nil && p.DeletionTimestamp.IsZero() && (p.Spec.NodeName != "" || unschedulable(p) != nil)
}

// replaceMember deletes a member's claim and Pod. The claim is released once
// its Pod is gone, and the StatefulSet recreates both.
func (r *Reconciler) replaceMember(ctx context.Context, c *corev1.PersistentVolumeClaim, p *corev1.Pod) error {
	if err := r.Delete(ctx, c, client.Preconditions{UID: new(c.UID)}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if p != nil && p.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, p, client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func waitingOnConfig(p *corev1.Pod) bool {
	return slices.ContainsFunc(p.Status.ContainerStatuses, func(s corev1.ContainerStatus) bool {
		return s.State.Waiting != nil && slices.Contains(configWaits, s.State.Waiting.Reason)
	})
}

// readySince reports when a ready Pod last became ready.
func readySince(p *corev1.Pod) (time.Time, bool) {
	if p == nil || !p.DeletionTimestamp.IsZero() {
		return time.Time{}, false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// notReadySince reports when a Pod that is not ready stopped being ready, or
// was created if it never was.
func notReadySince(p *corev1.Pod) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return p.CreationTimestamp.Time
}

// foreignClaim names the first claim at a member name below n that does not
// carry this fleet's label, so the StatefulSet never adopts another fleet's
// disk. It is empty for Bucket fleets, which have no claims.
func (r *Reconciler) foreignClaim(ctx context.Context, f *fleet.CelldFleet, n int32) (string, error) {
	if f.Spec.Profile != "PersistentFleet" {
		return "", nil
	}
	for ordinal := range n {
		claim := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: claimName(f, ordinal)}, claim)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if claim.Labels[FleetLabel] != string(f.UID) {
			return fmt.Sprintf("PVC %s exists and does not belong to this fleet; refusing to adopt it", claim.Name), nil
		}
	}
	return "", nil
}

// deleteClaims deletes every claim of a PersistentFleet whose workload is
// gone, including those of members removed by scale-in, and reports whether
// any remain.
func (r *Reconciler) deleteClaims(ctx context.Context, f *fleet.CelldFleet) (bool, error) {
	if f.Spec.Profile != "PersistentFleet" {
		return false, nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return false, err
	}
	for i := range claims.Items {
		c := &claims.Items[i]
		if c.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, c, client.Preconditions{UID: new(c.UID)}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	return len(claims.Items) > 0, nil
}
