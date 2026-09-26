package controller

import (
	"context"
	"fmt"
	"slices"
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
//     claim and Pod are deleted at once, and the StatefulSet recreates both.
//   - A member that has stayed down for the replacement delay, while every
//     other member has been ready for absorbWindow, is treated as lost. By then
//     celld has moved every session off it, so its claim and Pod are deleted
//     and it returns on a fresh disk. A member waiting on its image or
//     configuration, or already on a disk made after its Pod, is left alone:
//     a new disk would not help it.
//
// Each reconcile takes at most one of these actions.

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
// action is due but one member is down, waiting says when it will be replaced.
func (r *Reconciler) healMembers(ctx context.Context, f *fleet.CelldFleet, sts *appsv1.StatefulSet) (action, waiting string, err error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return "", "", err
	}
	// Only Pods the StatefulSet controls are members; anything else that
	// carries the fleet's label is ignored.
	var pods []corev1.Pod
	for _, p := range list.Items {
		if owner := metav1.GetControllerOf(&p); owner != nil && owner.UID == sts.UID {
			pods = append(pods, p)
		}
	}
	now := r.capacityNow()
	for i := range pods {
		p := &pods[i]
		if !p.DeletionTimestamp.IsZero() && now.Sub(p.DeletionTimestamp.Time) > terminationMargin {
			if err := r.Delete(ctx, p, client.GracePeriodSeconds(0), client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
				return "", "", err
			}
			return r.healed(f, "MemberForceDeleted", fmt.Sprintf("Force-deleted Pod %s: its node has not confirmed termination; the member returns on its own disk", p.Name))
		}
	}
	if f.Spec.Profile != "PersistentFleet" {
		return "", "", nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return "", "", err
	}
	podOf := map[string]*corev1.Pod{}
	for i := range pods {
		podOf[pods[i].Name] = &pods[i]
	}
	claimOf := map[string]*corev1.PersistentVolumeClaim{}
	for i := range claims.Items {
		claimOf[claims.Items[i].Name] = &claims.Items[i]
	}
	var down []int32
	for ordinal := range replicas(sts) {
		name := memberName(f, ordinal)
		p, c := podOf[name], claimOf[claimName(f, ordinal)]
		switch {
		case c != nil && !c.DeletionTimestamp.IsZero():
			// A replacement in progress: a scheduled Pod holds the claim until
			// it is gone. An unscheduled one does not.
			if p != nil && p.DeletionTimestamp.IsZero() && p.Spec.NodeName != "" {
				if err := r.Delete(ctx, p, client.Preconditions{UID: new(p.UID)}); err != nil && !apierrors.IsNotFound(err) {
					return "", "", err
				}
				return fmt.Sprintf("Replacing member %s: deleting its Pod so its old disk can be released", name), "", nil
			}
		case c != nil && c.Status.Phase == corev1.ClaimLost:
			if err := r.replaceMember(ctx, c, p); err != nil {
				return "", "", err
			}
			return r.healed(f, "MemberDiskLost", fmt.Sprintf("Replacing member %s: its volume no longer exists; celld records a bounded loss for any session with no other copy", name))
		}
		if p == nil || !p.DeletionTimestamp.IsZero() || !podReady(p) {
			down = append(down, ordinal)
		}
	}
	if len(down) != 1 {
		return "", "", nil
	}
	name := memberName(f, down[0])
	p, c := podOf[name], claimOf[claimName(f, down[0])]
	if p == nil || !p.DeletionTimestamp.IsZero() || c == nil || c.Status.Phase != corev1.ClaimBound || waitingOnConfig(p) {
		return "", "", nil
	}
	// The StatefulSet creates a claim just before or after the Pod that needs
	// it. A disk that new was made for this Pod; replacing it again would not
	// help.
	if !c.CreationTimestamp.Time.Before(p.CreationTimestamp.Add(-time.Minute)) {
		return "", "", nil
	}
	for ordinal := range replicas(sts) {
		if ordinal == down[0] {
			continue
		}
		if since, ok := readySince(podOf[memberName(f, ordinal)]); !ok || now.Sub(since) < absorbWindow {
			return "", "", nil
		}
	}
	due := notReadySince(p).Add(r.memberReplacementDelay())
	if now.Before(due) {
		return "", fmt.Sprintf("member %s is down; it is replaced on a fresh disk at %s unless it returns", name, due.UTC().Format(time.RFC3339)), nil
	}
	if err := r.replaceMember(ctx, c, p); err != nil {
		return "", "", err
	}
	return r.healed(f, "MemberReplaced", fmt.Sprintf("Replacing member %s: down since %s while the rest of the fleet is ready; it returns on a fresh disk", name, notReadySince(p).UTC().Format(time.RFC3339)))
}

func (r *Reconciler) healed(f *fleet.CelldFleet, reason, message string) (string, string, error) {
	r.eventf(f, corev1.EventTypeWarning, reason, "%s", message)
	return message, "", nil
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
