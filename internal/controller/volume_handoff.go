package controller

import (
	"context"
	"errors"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// verifyVolumeAttachment admits only the exclusive, healthy EBS CSI attachment
// to the observed target node. It is storage handoff evidence, never evidence
// that an old process died. That authority must already exist in the journal.
func (r *Reconciler) verifyVolumeAttachment(ctx context.Context, f *fleet.CelldFleet, m persistentMember) error {
	retained, err := r.retainedVolumeFor(ctx, f.Namespace, m.Node)
	if err != nil {
		return err
	}
	claim := retained.Claim
	// Handoff additionally demands ReadWriteOncePod; it does not re-check the
	// journal claim binding or the deletion timestamp.
	if !retained.sameDisk(m) || !slices.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}) {
		return errors.New("RWOP retained disk binding changed")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, pv); err != nil {
		return err
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "ebs.csi.aws.com" || !slices.Equal(pv.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}) || (pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode != corev1.PersistentVolumeFilesystem) {
		return errors.New("handoff requires filesystem RWOP EBS CSI")
	}
	// gp2/gp3 cannot enable EBS Multi-Attach. Do not infer single-attachment
	// hardware from RWOP alone, or admit imported/unknown storage classes.
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName == "" || pv.Spec.StorageClassName != *claim.Spec.StorageClassName {
		return errors.New("handoff storage class identity unavailable")
	}
	class := &storagev1.StorageClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: pv.Spec.StorageClassName}, class); err != nil {
		return err
	}
	singleAttachHardware := class.Provisioner == "ebs.csi.aws.com" && (class.Parameters["type"] == "gp2" || class.Parameters["type"] == "gp3")
	delayedBinding := class.VolumeBindingMode != nil && *class.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
	retainOnDelete := class.ReclaimPolicy != nil && *class.ReclaimPolicy == corev1.PersistentVolumeReclaimRetain
	if !singleAttachHardware || !delayedBinding || !retainOnDelete {
		return errors.New("handoff requires retained delayed-binding gp2/gp3 EBS; Multi-Attach capable or unknown storage is unsupported")
	}
	attachments := &storagev1.VolumeAttachmentList{}
	if err := r.List(ctx, attachments); err != nil {
		return err
	}
	count := 0
	for _, a := range attachments.Items {
		if a.Spec.Source.PersistentVolumeName == nil || *a.Spec.Source.PersistentVolumeName != pv.Name {
			continue
		}
		count++
		if a.Spec.Attacher != "ebs.csi.aws.com" || a.Spec.NodeName != m.Host || !a.DeletionTimestamp.IsZero() || !a.Status.Attached || a.Status.AttachError != nil || a.Status.DetachError != nil {
			return errors.New("EBS attachment handoff incomplete or ambiguous")
		}
	}
	if count != 1 {
		return errors.New("exactly one healthy EBS attachment required")
	}
	return nil
}

func (r *Reconciler) checkVolumeStartup(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, pod *corev1.Pod) error {
	if !slices.ContainsFunc(j.PersistentHistory, func(p persistentMember) bool { return p.Node == pod.Name }) {
		return nil
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.UID != j.WorkloadUID || owner.Kind != "StatefulSet" || !pod.DeletionTimestamp.IsZero() {
		return errors.New("retained-volume pod ownership changed")
	}
	if err := validatePersistentPod(evidenceRuntime(f, j), pod, r.Options); err != nil {
		return err
	}
	if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == "data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "data-"+pod.Name && !v.PersistentVolumeClaim.ReadOnly
	}) {
		return errors.New("retained-volume pod data association changed")
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "celld" && status.RestartCount != 0 {
			return errors.New("unexpected launcher restart during retained-volume startup")
		}
	}
	// An unscheduled/unstarted pod has no endpoint yet; normal reconciliation retries.
	if pod.Status.PodIP == "" {
		return nil
	}
	// Startup remains pinned to the original host incarnation. Disk-policy
	// cutover owns cross-host reuse; this path sends no launcher handoff grant.
	if podReady(pod) {
		return nil
	}
	state, err := r.callLauncher(ctx, f, pod, "", "")
	if err != nil {
		return err
	}
	if state.Phase == "Blocked" {
		return errors.New("launcher disk startup blocked: " + state.Error)
	}
	return nil
}

func latestPersistentMember(history []persistentMember, node string) persistentMember {
	for _, h := range slices.Backward(history) {
		if h.Node == node {
			return h
		}
	}
	return persistentMember{}
}
