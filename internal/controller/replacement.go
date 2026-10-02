package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	replacementFinalizer  = "celld.eric.dev/replacing-disk"
	replacementAnnotation = "celld.eric.dev/replacement-claim"
	replacementFleetLabel = "celld.eric.dev/replacement-fleet"
)

// The Pod holds its name until claim deletion is accepted. Preparation alone
// does not authorize deleting a disk: another controller may delete the Pod,
// or a Delete response may be lost. Only an acknowledged guarded Pod deletion
// lets this controller persist Committed. An interrupted preparation aborts
// conservatively, allowing a restart on the retained disk.
type replacementIntent struct {
	FleetUID             types.UID `json:"fleetUID"`
	ClaimUID             types.UID `json:"claimUID"`
	ClaimResourceVersion string    `json:"claimResourceVersion"`
	Committed            bool      `json:"committed"`
}

func replacementIntentOf(p *corev1.Pod) (replacementIntent, bool) {
	var intent replacementIntent
	err := json.Unmarshal([]byte(p.Annotations[replacementAnnotation]), &intent)
	return intent, err == nil && intent.FleetUID != "" && intent.ClaimUID != "" && intent.ClaimResourceVersion != ""
}

func setReplacementIntent(p *corev1.Pod, intent replacementIntent) {
	if intent.FleetUID == "" {
		intent.FleetUID = types.UID(p.Labels[FleetLabel])
	}
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[replacementFleetLabel] = string(intent.FleetUID)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	b, _ := json.Marshal(intent)
	p.Annotations[replacementAnnotation] = string(b)
}

// resumeReplacements runs before pause, deletion and prerequisite gates. It
// includes ordinals removed by scale-in, and never depends on a new rollout
// being settled. Otherwise our own finalizer could block StatefulSet GC.
func (r *Reconciler) resumeReplacements(ctx context.Context, f *fleet.CelldFleet) (bool, error) {
	if f.Spec.Profile != "PersistentFleet" {
		return false, nil
	}
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels{replacementFleetLabel: string(f.UID)}); err != nil {
		return false, err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if !slices.Contains(p.Finalizers, replacementFinalizer) {
			continue
		}
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" || owner.Name != f.Name || p.Labels[FleetLabel] != string(f.UID) {
			return true, r.releaseReplacement(ctx, p)
		}
		sts := &appsv1.StatefulSet{}
		err := r.Get(ctx, client.ObjectKeyFromObject(f), sts)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		// An absent/foreign workload cannot authorize disk replacement. It may
		// still have a dependent Pod held by us; release our hold only.
		if err != nil || sts.UID != owner.UID || owned(f, sts) != nil || !sts.DeletionTimestamp.IsZero() || paused(f) || !f.DeletionTimestamp.IsZero() {
			return true, r.releaseReplacement(ctx, p)
		}
		intent, ok := replacementIntentOf(p)
		if !ok || intent.FleetUID != f.UID || !intent.Committed || p.DeletionTimestamp.IsZero() {
			return true, r.releaseReplacement(ctx, p)
		}
		return true, r.finishReplacement(ctx, p)
	}
	return false, nil
}

// finishReplacement is authorized only by a held, terminating Pod whose
// guarded deletion was acknowledged. The original claim version is retained
// across restart: a repaired claim must not be deleted with a refreshed RV.
func (r *Reconciler) finishReplacement(ctx context.Context, p *corev1.Pod) error {
	intent, ok := replacementIntentOf(p)
	if !ok || intent.FleetUID != types.UID(p.Labels[FleetLabel]) || !intent.Committed || p.DeletionTimestamp.IsZero() || !slices.Contains(p.Finalizers, replacementFinalizer) {
		return r.releaseReplacement(ctx, p)
	}
	c := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: "data-" + p.Name}, c)
	if apierrors.IsNotFound(err) {
		return r.releaseReplacement(ctx, p)
	}
	if err != nil {
		return err
	}
	if c.UID != intent.ClaimUID || c.Labels[FleetLabel] != p.Labels[FleetLabel] {
		return r.releaseReplacement(ctx, p)
	}
	if c.DeletionTimestamp.IsZero() {
		if c.ResourceVersion != intent.ClaimResourceVersion {
			return r.releaseReplacement(ctx, p)
		}
		if err := r.Delete(ctx, c, client.Preconditions{UID: new(intent.ClaimUID), ResourceVersion: new(intent.ClaimResourceVersion)}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Do not wait for the claim to disappear: PVC protection needs this Pod
	// gone first. DeletionTimestamp (or an acknowledged Delete) is sufficient.
	return r.releaseReplacement(ctx, p)
}

func (r *Reconciler) releaseReplacement(ctx context.Context, observed *corev1.Pod) error {
	p := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(observed), p); err != nil {
		return client.IgnoreNotFound(err)
	}
	if p.UID != observed.UID {
		return nil
	}
	if !slices.Contains(p.Finalizers, replacementFinalizer) && p.Annotations[replacementAnnotation] == "" && p.Labels[replacementFleetLabel] == "" {
		return nil
	}
	p.Finalizers = slices.DeleteFunc(p.Finalizers, func(s string) bool { return s == replacementFinalizer })
	delete(p.Annotations, replacementAnnotation)
	delete(p.Labels, replacementFleetLabel)
	return r.Update(ctx, p)
}

// replaceMember first commits Pod deletion, retaining its name with a finalizer.
// Readiness/identity changes before this guarded Delete reject replacement while
// the claim is still intact. A successor cannot start using the old disk between
// the two deletions because its Pod name remains occupied.
func (r *Reconciler) replaceMember(ctx context.Context, c *corev1.PersistentVolumeClaim, observed *corev1.Pod) error {
	if observed == nil || !observed.DeletionTimestamp.IsZero() {
		return apierrors.NewConflict(corev1.Resource("pods"), c.Name, fmt.Errorf("replacement requires a live member Pod to hold its name"))
	}
	p := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(observed), p); err != nil {
		return err
	}
	if p.UID != observed.UID || p.ResourceVersion != observed.ResourceVersion || !p.DeletionTimestamp.IsZero() {
		return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("member changed before replacement preparation"))
	}
	if c.Name != "data-"+p.Name || c.Namespace != p.Namespace || c.UID == "" || c.ResourceVersion == "" || p.Labels[FleetLabel] == "" || c.Labels[FleetLabel] != p.Labels[FleetLabel] {
		return apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), c.Name, fmt.Errorf("replacement claim identity does not match its member"))
	}
	if slices.Contains(p.Finalizers, replacementFinalizer) {
		return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("replacement already prepared; reconcile its hold first"))
	}
	owner := metav1.GetControllerOf(p)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" {
		return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("replacement requires a StatefulSet member"))
	}
	intent := replacementIntent{FleetUID: types.UID(p.Labels[FleetLabel]), ClaimUID: c.UID, ClaimResourceVersion: c.ResourceVersion}
	p.Finalizers = append(p.Finalizers, replacementFinalizer)
	setReplacementIntent(p, intent)
	// Update is the preparation CAS and returns the RV for the guarded Delete.
	if err := r.Update(ctx, p); err != nil {
		return err
	}
	if err := r.Delete(ctx, p, client.Preconditions{UID: new(p.UID), ResourceVersion: new(p.ResourceVersion)}); err != nil {
		return err
	}
	current := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.UID != p.UID || current.DeletionTimestamp.IsZero() || !slices.Contains(current.Finalizers, replacementFinalizer) {
		return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("replacement Pod hold changed after deletion"))
	}
	currentOwner := metav1.GetControllerOf(current)
	if current.Labels[FleetLabel] != p.Labels[FleetLabel] || currentOwner == nil || currentOwner.UID != owner.UID || currentOwner.Name != owner.Name || currentOwner.Kind != owner.Kind || currentOwner.APIVersion != owner.APIVersion {
		if err := r.releaseReplacement(ctx, current); err != nil {
			return err
		}
		return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("member ownership changed before replacement commitment"))
	}
	intent.Committed = true
	setReplacementIntent(current, intent)
	if err := r.Update(ctx, current); err != nil {
		return err
	}
	return r.finishReplacement(ctx, current)
}
