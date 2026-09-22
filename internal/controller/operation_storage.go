package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const csiDeletionFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

func sameVolume(a, b volumeIdentity) bool {
	return a.Claim == b.Claim && a.ClaimUID == b.ClaimUID && a.Volume == b.Volume && a.VolumeUID == b.VolumeUID && a.Handle == b.Handle
}
func (r *Reconciler) verifyClaims(ctx context.Context, f *fleet.CelldFleet, s *fleetState) error {
	for name, uid := range s.Claims {
		c := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: name}, c); err != nil {
			return err
		}
		if c.UID != uid || !c.DeletionTimestamp.IsZero() || len(c.OwnerReferences) != 0 || c.Labels[FleetLabel] != string(f.UID) || c.Annotations["celld.eric.dev/storage-reservation"] != reservationName(f) {
			return errors.New("current PVC identity or ownership changed: " + name)
		}
	}
	return nil
}
func (r *Reconciler) targetVolume(ctx context.Context, f *fleet.CelldFleet, s *fleetState, pod string) (*volumeIdentity, error) {
	c := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "data-" + pod}, c); err != nil {
		return nil, err
	}
	if c.UID == "" || c.UID != s.Claims[c.Name] || !c.DeletionTimestamp.IsZero() || len(c.OwnerReferences) != 0 || c.Labels[FleetLabel] != string(f.UID) || c.Annotations["celld.eric.dev/storage-reservation"] != reservationName(f) || c.Spec.VolumeName == "" || c.Status.Phase != corev1.ClaimBound {
		return nil, errors.New("target claim binding unavailable")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: c.Spec.VolumeName}, pv); err != nil {
		return nil, err
	}
	v := &volumeIdentity{Claim: c.Name, ClaimUID: c.UID, ClaimVersion: c.ResourceVersion, Volume: pv.Name, VolumeUID: pv.UID, Handle: volumeHandle(pv), DeletionProtected: slices.Contains(pv.Finalizers, csiDeletionFinalizer)}
	if err := r.validateDisposableVolume(f, v, pv, false); err != nil {
		return nil, err
	}
	return v, nil
}
func (r *Reconciler) supportedCSI(driver string) bool {
	return driver == "ebs.csi.aws.com" || (r.Options.LocalTest && driver == localCSIDriver)
}
func volumeHandle(pv *corev1.PersistentVolume) string {
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return ""
	}
	return pv.Spec.CSI.Driver + ":" + pv.Spec.CSI.VolumeHandle
}
func (r *Reconciler) validateDisposableVolume(f *fleet.CelldFleet, v *volumeIdentity, pv *corev1.PersistentVolume, cleanupStarted bool) error {
	ref, csi := pv.Spec.ClaimRef, pv.Spec.CSI
	if pv.UID == "" || pv.UID != v.VolumeUID || pv.Name != v.Volume || ref == nil || ref.UID != v.ClaimUID || ref.Name != v.Claim || ref.Namespace != f.Namespace || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return errors.New("target PV identity, claim binding or Delete policy changed")
	}
	if csi == nil || !r.supportedCSI(csi.Driver) || volumeHandle(pv) == "" || volumeHandle(pv) != v.Handle || pv.Spec.StorageClassName != f.Spec.Storage.StorageClassName || pv.Annotations["pv.kubernetes.io/provisioned-by"] != csi.Driver {
		return errors.New("required dynamically provisioned CSI volume identity unavailable")
	}
	if !v.DeletionProtected || (!cleanupStarted && (!pv.DeletionTimestamp.IsZero() || !slices.Contains(pv.Finalizers, csiDeletionFinalizer))) {
		return errors.New("CSI backing-volume deletion finalizer is required before cleanup")
	}
	return nil
}

func (r *Reconciler) freshGrowth(ctx context.Context, f *fleet.CelldFleet, o *currentOperation) error {
	if f.Spec.Profile != "PersistentFleet" {
		return nil
	}
	from := o.From
	if o.Kind != "Scale" {
		from = 0
	}
	for i := from; i < o.To; i++ {
		c := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: fmt.Sprintf("data-%s-%d", f.Name, i)}, c)
		if err == nil {
			return errors.New("growth requires a fresh claim; retained disk reuse is unsupported")
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// cleanupClaims retains the current strict proof until CSI completes deletion.
// It never deletes a PV or mutates reclaim policy, finalizers or attachments.
func (r *Reconciler) cleanupClaims(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (bool, error) {
	s, o := h.j, h.j.Operation
	if o == nil || o.Phase != "DeleteClaims" {
		return false, errors.New("cleanup lacks current operation")
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err != nil {
		return false, err
	}
	if err := r.effectObserved(ctx, f, s, w, false); err != nil {
		return false, err
	}
	for i := range o.Targets {
		t := &o.Targets[i]
		if t.Storage == nil {
			continue
		}
		if t.Proof == nil || !validProof(o, *t, *t.Proof) {
			return false, errors.New("cleanup lacks exact strict proof")
		}
		v := t.Storage
		c := &corev1.PersistentVolumeClaim{}
		claimErr := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: v.Claim}, c)
		if claimErr != nil && !apierrors.IsNotFound(claimErr) {
			return false, claimErr
		}
		if claimErr == nil && (c.UID != v.ClaimUID || c.Spec.VolumeName != v.Volume || len(c.OwnerReferences) != 0 || c.Labels[FleetLabel] != string(f.UID) || c.Annotations["celld.eric.dev/storage-reservation"] != reservationName(f)) {
			return false, errors.New("claim replacement or binding drift during cleanup")
		}
		pv := &corev1.PersistentVolume{}
		volumeErr := r.Get(ctx, client.ObjectKey{Name: v.Volume}, pv)
		if volumeErr != nil && !apierrors.IsNotFound(volumeErr) {
			return false, volumeErr
		}
		if volumeErr == nil {
			if err := r.validateDisposableVolume(f, v, pv, v.CleanupStarted && (apierrors.IsNotFound(claimErr) || !c.DeletionTimestamp.IsZero())); err != nil {
				return false, err
			}
		} else if !v.CleanupStarted {
			return false, errors.New("PV disappeared before authorized cleanup")
		}
		if apierrors.IsNotFound(claimErr) {
			if !v.CleanupStarted {
				return false, errors.New("PVC disappeared before authorized cleanup")
			}
			if volumeErr == nil {
				return false, nil
			}
			detached, err := r.volumeDetached(ctx, v.Volume)
			if err != nil || !detached {
				return false, err
			}
			delete(s.Claims, v.Claim)
			continue
		}
		if !v.CleanupStarted && !c.DeletionTimestamp.IsZero() {
			return false, errors.New("PVC deletion preceded authorized cleanup")
		}
		if !c.DeletionTimestamp.IsZero() {
			return false, nil
		}
		if volumeErr != nil {
			return false, errors.New("bound PV disappeared while claim remains")
		}
		// Persist intent and the exact claim resourceVersion before issuing deletion.
		// A lost response can then be recovered by observing these same objects.
		if !v.CleanupStarted || v.ClaimVersion != c.ResourceVersion {
			v.CleanupStarted = true
			v.ClaimVersion = c.ResourceVersion
			return false, r.saveState(ctx, h.res, s)
		}
		all := &corev1.PodList{}
		if err := r.List(ctx, all, client.InNamespace(f.Namespace)); err != nil {
			return false, err
		}
		for _, p := range all.Items {
			for _, vol := range p.Spec.Volumes {
				if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == c.Name {
					return false, errors.New("claim is still referenced by a pod")
				}
			}
		}
		live := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(h.res), live); err != nil {
			return false, err
		}
		if live.UID != h.res.UID || live.ResourceVersion != h.res.ResourceVersion {
			return false, errors.New("cleanup reservation changed")
		}
		r.faultPoint("before-cleanup")
		err := r.Delete(ctx, c, client.Preconditions{UID: &v.ClaimUID, ResourceVersion: &v.ClaimVersion})
		r.faultPoint("after-cleanup")
		return false, client.IgnoreNotFound(err)
	}
	return true, r.saveState(ctx, h.res, s)
}
func (r *Reconciler) volumeDetached(ctx context.Context, volume string) (bool, error) {
	// A terminating or detached attachment object can still represent unfinished
	// CSI teardown. Only absence completes this operation; never force detach.
	attachments := &storagev1.VolumeAttachmentList{}
	if err := r.List(ctx, attachments); err != nil {
		return false, err
	}
	for _, a := range attachments.Items {
		if a.Spec.Source.PersistentVolumeName != nil && *a.Spec.Source.PersistentVolumeName == volume {
			return false, nil
		}
	}
	return true, nil
}
func diskCleanupPending(s *fleetState) bool {
	if s == nil || s.Operation == nil || s.Operation.Phase != "DeleteClaims" {
		return false
	}
	return slices.ContainsFunc(s.Operation.Targets, func(t operationTarget) bool { return t.Storage != nil })
}
func (r *Reconciler) admitNewClaims(ctx context.Context, f *fleet.CelldFleet, h *loadedState) error {
	if f.Spec.Profile != "PersistentFleet" {
		return nil
	}
	s, o := h.j, h.j.Operation
	changed := false
	from := o.From
	if o.Kind != "Scale" {
		from = 0
	}
	for i := from; i < o.To; i++ {
		name := fmt.Sprintf("data-%s-%d", f.Name, i)
		c := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: name}, c); err != nil {
			return err
		}
		if c.UID == "" || !c.DeletionTimestamp.IsZero() || c.Labels[FleetLabel] != string(f.UID) || len(c.OwnerReferences) != 0 || c.Annotations["celld.eric.dev/storage-reservation"] != h.res.Name {
			return errors.New("new claim ownership unavailable")
		}
		if uid := s.Claims[name]; uid != "" && uid != c.UID {
			return errors.New("new claim replaced after admission")
		}
		// A completed old disk can never be admitted again under a new operation.
		for _, t := range o.Targets {
			if t.Storage != nil && t.Storage.ClaimUID == c.UID {
				return errors.New("fresh growth reused retired claim")
			}
		}
		if s.Claims[name] == "" {
			s.Claims[name] = c.UID
			changed = true
		}
	}
	if changed {
		return r.saveState(ctx, h.res, s)
	}
	return r.verifyClaims(ctx, f, s)
}
func (r *Reconciler) scheduleCurrent(ctx context.Context, f *fleet.CelldFleet, s *fleetState) error {
	if orderedBucket(f) {
		return r.scheduleOrderedBucket(ctx, f, s)
	}
	if f.Spec.Profile != "PersistentFleet" || r.Options.LauncherImage == "" {
		return nil
	}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return err
	}
	for i := range pods {
		p := &pods[i]
		if !slices.ContainsFunc(p.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == launcherGate }) {
			continue
		}
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.UID != s.WorkloadUID || p.Spec.NodeName != "" || !p.DeletionTimestamp.IsZero() {
			return errors.New("gated pod identity changed")
		}
		c := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "data-" + p.Name}, c); err != nil {
			return err
		}
		if c.UID == "" || s.Claims[c.Name] != c.UID {
			return errors.New("gated pod lacks admitted claim identity")
		}
		// The launcher still refuses foreign host/boot reuse before child startup.
		// No controller grant or old runtime metadata can override that exclusion.
		p.Spec.SchedulingGates = slices.DeleteFunc(p.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == launcherGate })
		if err := r.Update(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
