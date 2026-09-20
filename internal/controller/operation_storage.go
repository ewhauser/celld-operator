package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
	ref := pv.Spec.ClaimRef
	if pv.UID == "" || !pv.DeletionTimestamp.IsZero() || ref == nil || ref.UID != c.UID || ref.Name != c.Name || ref.Namespace != c.Namespace || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return nil, errors.New("target PV identity, claim binding or Retain policy changed")
	}
	handle := ""
	if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle != "" && (pv.Spec.CSI.Driver == "ebs.csi.aws.com" || (r.Options.LocalTest && pv.Spec.CSI.Driver == localCSIDriver)) {
		handle = pv.Spec.CSI.Driver + ":" + pv.Spec.CSI.VolumeHandle
	}
	if r.Options.LocalTest && pv.Spec.HostPath != nil && pv.Spec.HostPath.Path != "" {
		handle = "local-hostPath:" + pv.Spec.HostPath.Path
	}
	if handle == "" {
		return nil, errors.New("qualified volume identity unavailable")
	}
	return &volumeIdentity{Claim: c.Name, ClaimUID: c.UID, ClaimVersion: c.ResourceVersion, Volume: pv.Name, VolumeUID: pv.UID, Handle: handle}, nil
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
func (r *Reconciler) cleanupClaims(ctx context.Context, f *fleet.CelldFleet, h *loadedState) (bool, error) {
	s, o := h.j, h.j.Operation
	for i := range o.Targets {
		t := &o.Targets[i]
		if t.Storage == nil {
			continue
		}
		v := t.Storage
		c := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: v.Claim}, c)
		if apierrors.IsNotFound(err) {
			delete(s.Claims, v.Claim)
			s.DiskCleanupPending = true
			continue
		}
		if err != nil {
			return false, err
		}
		if c.UID != v.ClaimUID {
			return false, errors.New("claim replacement during cleanup")
		}
		// Validate PV/handle even during PVC termination; deleting the PVC does not
		// authorize deleting a PV or changing its reclaim policy.
		if !c.DeletionTimestamp.IsZero() {
			return false, nil
		}
		current, err := r.targetVolume(ctx, f, s, t.Pod)
		if err != nil {
			return false, err
		}
		if !sameVolume(*v, *current) {
			return false, errors.New("disk changed during cleanup")
		}
		if v.ClaimVersion != c.ResourceVersion {
			v.ClaimVersion = c.ResourceVersion
			return false, r.saveState(ctx, h.res, s)
		}
		// Recheck no pod can still reference the claim. The workload remains at the
		// exact stopped count until this operation has observed every deletion.
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
		r.faultPoint("before-cleanup")
		err = r.Delete(ctx, c, client.Preconditions{UID: &v.ClaimUID, ResourceVersion: &v.ClaimVersion})
		r.faultPoint("after-cleanup")
		return false, err
	}
	return true, r.saveState(ctx, h.res, s)
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
