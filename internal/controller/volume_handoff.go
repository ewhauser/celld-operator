package controller

import (
	"context"
	"errors"
	"slices"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
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
	if class.Provisioner != "ebs.csi.aws.com" || (class.Parameters["type"] != "gp2" && class.Parameters["type"] != "gp3") || class.VolumeBindingMode == nil || *class.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer || class.ReclaimPolicy == nil || *class.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
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

func handoffPredecessor(j *lifecycleJournal, pod *corev1.Pod, state launcher.State, host *corev1.Node) (persistentMember, error) {
	predecessor, found := latestPersistentMember(j.PersistentHistory, pod.Name)
	for _, p := range j.PersistentHistory {
		if p.Node != pod.Name {
			continue
		}
		// A superseded survivor is resolved by the successor launcher's exclusive
		// lock on the same host incarnation, exactly as everywhere else in the
		// journal. Only the predecessor below must be a positive retirement.
		if !resolvedMember(p) {
			return predecessor, errors.New("unresolved historical disk writer")
		}

	}
	if !found || !predecessor.Stopped || !predecessor.Retired || !predecessor.RestartDenied || predecessor.DiskID == "" || predecessor.DiskID != state.DiskID || predecessor.Host+"\n"+predecessor.BootID != state.PreviousHost || predecessor.PodUID == string(pod.UID) || predecessor.Generation == state.Generation || predecessor.Zone == "" || predecessor.Zone != host.Labels[corev1.LabelTopologyZone] || !healthyHost(host) || state.BootID != host.Status.NodeInfo.BootID {
		return predecessor, errors.New("handoff lacks exact retired disk, host or zone continuity")
	}
	return predecessor, nil
}

func (r *Reconciler) authorizeVolumeHandoff(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, pod *corev1.Pod) error {
	if !slices.ContainsFunc(j.PersistentHistory, func(p persistentMember) bool { return p.Node == pod.Name }) {
		return nil
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.UID != j.WorkloadUID || owner.Kind != "StatefulSet" || !pod.DeletionTimestamp.IsZero() {
		return errors.New("handoff pod ownership changed")
	}
	if err := validatePersistentPod(evidenceRuntime(f, j), pod, r.Options); err != nil {
		return err
	}
	if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == "data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "data-"+pod.Name && !v.PersistentVolumeClaim.ReadOnly
	}) {
		return errors.New("handoff pod data volume association changed")
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "celld" && status.RestartCount != 0 {
			return errors.New("unexpected launcher restart cannot authorize handoff")
		}
	}
	// An unscheduled/unstarted pod has no endpoint yet; normal reconciliation retries.
	if pod.Status.PodIP == "" {
		return nil
	}
	state, err := r.callLauncher(ctx, f, pod, "", "")
	if err != nil {
		return err
	}
	if state.Phase != "WaitingForHandoff" {
		return nil
	}
	if r.Options.LocalTest {
		return errors.New("cross-host handoff requires real EBS CSI")
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return err
	}
	previous, err := handoffPredecessor(j, pod, state, node)
	if err != nil {
		return err
	}
	reactivating := j.Operation != nil && j.Operation.Phase == "Reactivating" && reactivatedNode(f.Name, pod.Name, j.Operation.From, j.Operation.To)
	maintenance := j.Maintenance
	restarting := maintenance != nil && maintenance.Kind == "Restart" && maintenance.Phase == "Recovering" && maintenance.Index < len(maintenance.Targets) && maintenance.Targets[maintenance.Index].Name == pod.Name && maintenance.Targets[maintenance.Index].UID != pod.UID
	coordinated := maintenance != nil && maintenance.Coordinated && maintenance.Phase == "Resuming" && reactivatedNode(f.Name, pod.Name, 0, maintenance.TargetReplicas)
	if !reactivating && !restarting && !coordinated {
		return errors.New("handoff requires durable reactivation authority")
	}
	if j.Loss != "" || r.Evidence == nil {
		return errors.New("loss fence or missing live storage evidence blocks handoff")
	}
	inventory, err := r.readInventory(ctx, f, runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool {
		return n.Name == previous.Node && n.Generation == previous.Generation && n.ExpiresMS <= uint64(r.capacityNow().UnixMilli()) && n.Epoch >= previous.Epoch && (n.LogState == "sealed" || (n.LogState == "" && previous.Epoch == 0))
	}) {
		return errors.New("retired predecessor revived or recovery evidence changed")
	}
	target := previous
	target.Host = node.Name
	if err := r.verifyVolumeAttachment(ctx, f, target); err != nil {
		return err
	}
	// The fresh signed destination challenge is the one-use grant. A crash or a
	// successor changes invocation/generation, so a delayed grant cannot launch it.
	_, err = r.launcherRequest(ctx, f, pod, "", "", &launcher.Handoff{Invocation: state.Invocation, Generation: state.Generation, PodUID: string(pod.UID), Host: node.Name, BootID: state.BootID, DiskID: state.DiskID, PreviousHost: state.PreviousHost})
	return err
}

func latestPersistentMember(history []persistentMember, node string) (persistentMember, bool) {
	for _, h := range slices.Backward(history) {
		if h.Node == node {
			return h, true
		}
	}
	return persistentMember{}, false
}
