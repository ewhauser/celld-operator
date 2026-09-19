package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// persistentRecovery is the durable record of one admitted PersistentFleet
// invocation whose exact host incarnation became unavailable outside any
// operation, and of the reactivation of its retained disk after an exact
// infrastructure fence. Absence is never evidence: the record exists only once
// an administrator has authorized fencing that exact invocation, and the only
// stop authority it can ever hold is a positive terminated-instance receipt.
type persistentRecovery struct {
	ID        string
	Member    persistentMember
	Phase     string // Fencing, Reactivating
	StartedAt time.Time
}

const recoveryPrefix = "recover:"

// recoveryID names the exact admitted invocation. An authorization written for
// it cannot drift to a successor generation on the same ordinal.
func recoveryID(m persistentMember) string { return recoveryPrefix + m.Node + ":" + m.Generation }

func validatePersistentRecovery(j *lifecycleJournal) error {
	rec := j.Recovery
	if rec == nil {
		return nil
	}
	if rec.ID != recoveryID(rec.Member) || (rec.Phase != "Fencing" && rec.Phase != "Reactivating") || rec.StartedAt.IsZero() || rec.Member.DiskID == "" || rec.Member.Zone == "" || rec.Member.ProviderID == "" {
		return errors.New("invalid PersistentFleet recovery record")
	}
	latest, ok := latestPersistentMember(j.PersistentHistory, rec.Member.Node)
	if !ok || !sameInvocation(latest, rec.Member) {
		return errors.New("recovery record names an invocation that is not the latest admitted writer")
	}
	if rec.Phase == "Reactivating" && (!latest.Stopped || !latest.RestartDenied || !latest.Retired || !certifiedInfrastructureFence(j, rec.Member)) {
		return errors.New("recovery reactivation lacks a positive infrastructure fence receipt")
	}
	return nil
}

// admittedSurvivors returns, for every ordinal below the applied count, the
// latest history entry when it is still an unresolved live invocation.
func admittedSurvivors(f *fleet.CelldFleet, j *lifecycleJournal) []persistentMember {
	var out []persistentMember
	for ordinal := range j.Applied {
		latest, ok := latestPersistentMember(j.PersistentHistory, fmt.Sprintf("%s-%d", f.Name, ordinal))
		if ok && !resolvedMember(latest) {
			out = append(out, latest)
		}
	}
	return out
}

// admitPersistentMember records a running authenticated invocation. It never
// resolves, supersedes or retires history: a running generation that follows
// an unresolved admitted writer on the same ordinal is an integrity finding.
func admitPersistentMember(j *lifecycleJournal, m persistentMember) (bool, error) {
	if slices.ContainsFunc(j.PersistentHistory, func(p persistentMember) bool { return p.Node == m.Node && p.Generation == m.Generation }) {
		return false, nil
	}
	if latest, ok := latestPersistentMember(j.PersistentHistory, m.Node); ok && !resolvedMember(latest) {
		return false, errors.New("running invocation of " + m.Node + " succeeded an unresolved admitted writer; investigate before any lifecycle action")
	}
	j.PersistentHistory = append(j.PersistentHistory, m)
	return true, nil
}

// admitPersistentMembers captures every currently running invocation so that a
// later uncertain failure has an exact identity (pod, launcher generation, host
// incarnation, instance, disk nonce, EBS volume) to fence. It is observational
// and best effort: an unconverged or unreachable fleet records nothing.
func (r *Reconciler) admitPersistentMembers(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) (bool, error) {
	members, err := r.persistentMembers(ctx, f, j, j.Applied, false)
	if err != nil {
		return false, nil //nolint:nilerr // an unconverged or unreachable fleet simply records nothing
	}
	changed := false
	for _, m := range members {
		added, err := admitPersistentMember(j, m)
		if err != nil {
			return changed, err
		}
		changed = changed || added
	}
	return changed, nil
}

// uncertainMember reports whether an admitted survivor's exact host incarnation
// is unavailable: its Node is gone, re-registered or rebooted, or NotReady while
// the launcher cannot be reached. A launcher that answers is never uncertain,
// whatever Kubernetes reports: the kernel holding the volume lock is alive.
func (r *Reconciler) uncertainMember(ctx context.Context, f *fleet.CelldFleet, m persistentMember) (bool, string, error) {
	node := &corev1.Node{}
	err := r.Get(ctx, client.ObjectKey{Name: m.Host}, node)
	switch {
	case apierrors.IsNotFound(err):
		return true, "host " + m.Host + " no longer exists", nil
	case err != nil:
		return false, "", err
	case string(node.UID) != m.HostUID:
		return true, "host " + m.Host + " was re-registered with another identity", nil
	case node.Status.NodeInfo.BootID != m.BootID:
		return true, "host " + m.Host + " rebooted", nil
	case healthyHost(node):
		return false, "", nil
	}
	pod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: m.Node}, pod)
	if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != m.PodUID) {
		return true, "host " + m.Host + " is not ready and pod " + m.Node + " is gone", nil
	}
	if err != nil {
		return false, "", err
	}
	if pod.Status.PodIP != "" {
		if _, err := r.callLauncher(ctx, f, pod, "", ""); err == nil {
			return false, "", nil
		}
	}
	return true, "host " + m.Host + " is not ready and the launcher of " + m.Node + " is unreachable", nil
}

// recoveryFenceAdmissible is the precondition for recording infrastructure
// fencing intent outside a contraction: the exact recovery member, still the
// latest unresolved writer, still unreachable, with nothing else in flight.
func (r *Reconciler) recoveryFenceAdmissible(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, member persistentMember) error {
	rec := j.Recovery
	if rec == nil || rec.Phase != "Fencing" || !sameInvocation(rec.Member, member) || rec.Member.ProviderID != member.ProviderID {
		return errors.New("fencing requires a durable recovery record for this exact invocation")
	}
	if j.Operation != nil || j.Maintenance != nil || j.BucketMigration != nil {
		return errors.New("recovery fencing cannot overlap another lifecycle operation")
	}
	latest, ok := latestPersistentMember(j.PersistentHistory, member.Node)
	if !ok || !sameInvocation(latest, member) || resolvedMember(latest) {
		return errors.New("recovery member is not the latest unresolved admitted invocation")
	}
	uncertain, _, err := r.uncertainMember(ctx, f, member)
	if err != nil {
		return err
	}
	if !uncertain {
		return errors.New("admitted invocation is reachable again; refusing to terminate a live host")
	}
	return nil
}

// persistentRecovery runs before scheduling so a gated replacement that cannot
// be placed on its lost host does not hide the member's state. With a record in
// flight it owns the reconcile; otherwise it admits running invocations and
// reports, never repairs, an unreachable one.
func (r *Reconciler) persistentRecovery(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	if j.Recovery != nil {
		return r.executePersistentRecovery(ctx, f, res, j, w)
	}
	if j.Operation != nil || j.Maintenance != nil || j.BucketMigration != nil {
		return ctrl.Result{}, false, nil
	}
	report := func(reason, message string) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, reason, message, 0, false)
		return result, true, err
	}
	changed, err := r.admitPersistentMembers(ctx, appliedRuntime(f, j), j)
	if err != nil {
		return report("PersistentAdmissionBlocked", err.Error())
	}
	if changed {
		// Durable before anything else in this reconcile can act on the members.
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	for _, m := range admittedSurvivors(f, j) {
		uncertain, detail, err := r.uncertainMember(ctx, appliedRuntime(f, j), m)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if !uncertain {
			continue
		}
		id := recoveryID(m)
		if f.Annotations[fenceRequestKey] == id {
			if j.Loss != "" {
				return report("PossibleDataLoss", j.Loss)
			}
			if maintenanceFence(f) != "" {
				return report("MaintenancePaused", "Recovery fencing is a new action; it waits while the fleet is paused or deleting")
			}
			if r.Infrastructure == nil || r.Options.LocalTest || r.Options.FencingAccount == "" || r.Options.FencingRegion == "" {
				return report("PersistentMemberUncertain", "Fencing "+m.Node+" was requested, but opt-in EC2 fencing is not configured on this operator; the retained disk and authority are kept")
			}
			j.Recovery = &persistentRecovery{ID: id, Member: m, Phase: "Fencing", StartedAt: r.capacityNow()}
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
			return report("LifecycleProgress", "Durable recovery record created for "+m.Node+"; exact EC2 fencing follows on the next reconcile")
		}
		return report("PersistentMemberUncertain", fmt.Sprintf("Admitted member %s (generation %s, pod %s) is unreachable: %s. Its retained disk and authority are kept and nothing is repaired automatically. To fence the exact instance %s and reactivate the disk on another node in %s, verify the instance tags and set annotation %s=%s", m.Node, m.Generation, m.PodUID, detail, m.ProviderID, m.Zone, fenceRequestKey, id))
	}
	return ctrl.Result{}, false, nil
}

func (r *Reconciler) executePersistentRecovery(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	rec := j.Recovery
	old := rec.Member
	report := func(reason, message string) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, reason, message, 0, false)
		return result, true, err
	}
	save := func(message string) (ctrl.Result, bool, error) {
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		return report("LifecycleProgress", message)
	}
	fail := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		return report("PersistentRecoveryBlocked", err.Error())
	}
	if result, handled, err := r.observeEvidence(ctx, f, w, res, j); handled || err != nil {
		return result, handled, err
	}
	if j.Loss != "" {
		return report("PossibleDataLoss", j.Loss)
	}
	switch rec.Phase {
	case "Fencing":
		intent := slices.ContainsFunc(j.InfrastructureFences, func(receipt infrastructureFence) bool {
			return receipt.Operation == rec.ID && sameInvocation(receipt.Member, old)
		})
		if !intent {
			// Nothing has been issued: withdrawing the annotation withdraws the request.
			if f.Annotations[fenceRequestKey] != rec.ID {
				j.Recovery = nil
				return save("Recovery request withdrawn before any infrastructure action; the member remains unreachable")
			}
			if maintenanceFence(f) != "" {
				return report("MaintenancePaused", "Recovery fencing has not been issued; it waits while the fleet is paused or deleting")
			}
		}
		fenced, err := r.ensureInfrastructureFence(ctx, f, res, j, old, rec.ID)
		if err != nil {
			return fail(err)
		}
		if !fenced {
			return report("InfrastructureFencing", "Waiting for positive exact EC2 termination of "+old.ProviderID+" hosting "+old.Node)
		}
		index := slices.IndexFunc(j.PersistentHistory, func(p persistentMember) bool { return sameInvocation(p, old) })
		if index < 0 {
			return fail(errors.New("fenced invocation is missing from admitted history"))
		}
		reader, err := r.Evidence.reader(ctx, f)
		if err != nil {
			return fail(err)
		}
		adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
		if err != nil {
			return fail(err)
		}
		inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
		if err != nil {
			return fail(err)
		}
		// Irreversible instance termination is the stop and the restart denial.
		// Bind the retirement to the highest epoch observed so far, so the later
		// seal check cannot be satisfied by an older log record.
		retired := &j.PersistentHistory[index]
		for _, n := range inventory.Nodes {
			if n.Name == old.Node && n.Generation == old.Generation {
				retired.Epoch = max(retired.Epoch, n.Epoch)
				retired.Ensemble = slices.Clone(n.Ensemble)
			}
		}
		retired.Stopped, retired.RestartDenied, retired.Retired = true, true, true
		rec.Member = *retired
		rec.Phase = "Reactivating"
		return save("Exact instance terminated; " + old.Node + " is retired and its retained disk may be reactivated in " + old.Zone)
	case "Reactivating":
		pod := &corev1.Pod{}
		err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: old.Node}, pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return fail(err)
		}
		if err == nil && string(pod.UID) == old.PodUID {
			// The pod belonged to a positively terminated instance. Removing it
			// without a grace period is the documented safe case; kubelet cannot
			// answer, and the StatefulSet recreates the ordinal behind the gate.
			uid := types.UID(old.PodUID)
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &uid}, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
				return fail(err)
			}
			return report("LifecycleProgress", "Removed the pod of the terminated instance; waiting for the StatefulSet to recreate "+old.Node)
		}
		if err := r.schedulePersistent(ctx, appliedRuntime(f, j), res, j); err != nil {
			if _, loss := errors.AsType[*v050.LossError](err); loss {
				return r.recordLoss(ctx, f, w, res, j, err.Error())
			}
			return report("PersistentSchedulingBlocked", err.Error())
		}
		return r.finishRecovery(ctx, f, res, j, w)
	default:
		return fail(errors.New("invalid persistent recovery phase"))
	}
}

// finishRecovery admits the replacement invocation: new pod and generation on a
// different host incarnation in the recorded zone, unchanged claim, volume,
// handle and disk nonce, exclusive healthy EBS attachment, a live node record,
// and a predecessor that is still expired and sealed. Survivors are re-admitted
// under the ordinary rule; a survivor that changed to an unresolved writer blocks.
func (r *Reconciler) finishRecovery(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	rec := j.Recovery
	old := rec.Member
	fail := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		result, e := r.report(ctx, f, "PersistentRecoveryBlocked", err.Error(), 0, false)
		return result, true, e
	}
	if replicas(w) != j.Applied {
		return fail(errors.New("recovery replica authority changed"))
	}
	members, err := r.persistentMembers(ctx, appliedRuntime(f, j), j, j.Applied, false)
	if err != nil {
		return fail(fmt.Errorf("waiting for the replacement of %s: %w", old.Node, err))
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return fail(err)
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return fail(err)
	}
	inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
	if err != nil {
		return fail(err)
	}
	// The predecessor's expired lease and sealed log were required by the handoff
	// grant before celld could start on another host; once the successor runs,
	// the node record carries its generation instead. A live record for the
	// fenced generation would mean the grant was bypassed.
	now := uint64(r.capacityNow().UnixMilli())
	if slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool {
		return n.Name == old.Node && n.Generation == old.Generation && n.ExpiresMS > now
	}) {
		return fail(errors.New("fenced predecessor lease revived"))
	}
	var replacement persistentMember
	for i := range members {
		m := &members[i]
		if m.Node != old.Node {
			continue
		}
		sameHost := m.Host == old.Host && m.HostUID == old.HostUID && m.BootID == old.BootID
		if m.PodUID == old.PodUID || m.Generation == old.Generation || m.ClaimUID != old.ClaimUID || m.VolumeUID != old.VolumeUID || m.VolumeHandle != old.VolumeHandle || m.DiskID != old.DiskID || m.Zone != old.Zone || sameHost || r.Options.LocalTest {
			return fail(errors.New("replacement lacks retained disk, zone and new host continuity"))
		}
		if err := r.verifyVolumeAttachment(ctx, f, *m); err != nil {
			return fail(err)
		}
		index := slices.IndexFunc(inventory.Nodes, func(n v050.Node) bool {
			return n.Name == m.Node && n.Generation == m.Generation && n.ExpiresMS > now
		})
		if index < 0 {
			return fail(errors.New("replacement generation has no live node record"))
		}
		m.Epoch = inventory.Nodes[index].Epoch
		m.Ensemble = slices.Clone(inventory.Nodes[index].Ensemble)
		replacement = *m
	}
	if replacement.Generation == "" {
		return fail(errors.New("replacement member missing"))
	}
	after, err := r.persistentMembers(ctx, appliedRuntime(f, j), j, j.Applied, false)
	if err != nil {
		return fail(err)
	}
	for _, m := range members {
		if !slices.ContainsFunc(after, func(current persistentMember) bool { return sameInvocation(m, current) }) {
			return fail(errors.New("runtime changed while observing recovery evidence"))
		}
	}
	if inventory.ObservedAt.After(r.capacityNow()) || r.capacityNow().Sub(inventory.ObservedAt) > 5*time.Second {
		return fail(errors.New("recovery evidence expired"))
	}
	for _, m := range members {
		if _, err := admitPersistentMember(j, m); err != nil {
			return fail(err)
		}
	}
	j.History = append(j.History, lifecycleCompletion{ID: rec.ID, TargetPod: old.Node, TargetUID: old.PodUID, TargetGeneration: old.Generation, From: j.Applied, To: j.Applied, EvidenceAt: inventory.ObservedAt, Outcome: "InfrastructureFencedMemberReactivated"})
	j.Recovery = nil
	if err := r.saveJournal(ctx, res, j); err != nil {
		return ctrl.Result{}, true, err
	}
	result, e := r.report(ctx, f, "LifecycleProgress", "Retained disk of "+old.Node+" reactivated on "+replacement.Host+" after exact instance fencing", 0, false)
	return result, true, e
}
