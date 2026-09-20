package controller

import (
	"context"
	"errors"
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

func (r *Reconciler) executePersistentMaintenance(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object, block func(error) (ctrl.Result, bool, error)) (ctrl.Result, bool, error) {
	m := j.Maintenance
	p := &maintenancePass{f: f, res: res, j: j, w: w, m: m, block: block}
	p.save = func() (ctrl.Result, bool, error) {
		return r.saveMaintenance(ctx, f, res, j)
	}
	if !m.Coordinated && m.Kind == "Restart" && m.Phase == "Capture" && j.Applied < 3 && coordinatedDowntime(f) {
		m.Coordinated, m.TargetReplicas = true, j.Applied
		return p.save()
	}
	if m.Coordinated {
		return r.executeCoordinatedPersistent(ctx, f, res, j, w, block)
	}
	if m.Kind == "Delete" {
		return r.executePersistentShutdown(ctx, f, res, j, w, block)
	}
	if j.Applied < 3 {
		return block(errors.New("restart requires at least two survivors with qualified follower retirement"))
	}
	p.view = *j
	p.view.Operation = &lifecycleOperation{ID: m.ID, From: j.Applied, To: j.Applied - 1, PersistentMembers: m.Persistent}
	if m.Index < len(m.Targets) {
		p.view.Operation.TargetPod = m.Targets[m.Index].Name
		p.view.Operation.TargetUID = string(m.Targets[m.Index].UID)
	}
	switch m.Phase {
	case "Capture", "Next":
		return r.executePersistentCapture(ctx, p)
	case "Stopping":
		return r.executePersistentStopping(ctx, p)
	case "Authorized":
		return r.executePersistentAuthorized(ctx, p)
	case "Recovering":
		return r.executePersistentRecovering(ctx, p)
	default:
		return block(errors.New("invalid persistent restart phase"))
	}
}

func (r *Reconciler) executePersistentCapture(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, j, w, m, save, block := p.f, p.j, p.w, p.m, p.save, p.block
	if m.Phase == "Capture" {
		members, err := r.persistentMembers(ctx, f, &p.view, j.Applied, false)
		if err != nil {
			return block(err)
		}
		// Capture is replayed whenever a later step in this pass blocks, so the
		// inventory is rebuilt and replaced rather than appended to. Appending
		// duplicated every target and wedged the journal on its next load.
		targets := make([]maintenanceTarget, 0, len(members))
		for _, member := range members {
			targets = append(targets, maintenanceTarget{Name: member.Node, UID: types.UID(member.PodUID)})
		}
		m.Targets = targets
		if len(m.Targets) > 0 {
			p.view.Operation.TargetPod = m.Targets[0].Name
		}
	}
	if m.Index == len(m.Targets) {
		j.CompletedRestarts = append(j.CompletedRestarts, m.Token)
		r.recordCompletion(f, j, lifecycleCompletion{ID: m.ID, From: j.Applied, To: j.Applied, EvidenceAt: r.capacityNow(), Outcome: "RestartComplete"})
		j.Maintenance = nil
		j.Request = nil
		return save()
	}
	members, _, err := r.assessPersistent(ctx, f, &p.view, false, false)
	if err != nil {
		return block(err)
	}
	target := m.Targets[m.Index]
	if !slices.ContainsFunc(members, func(p persistentMember) bool { return p.Node == target.Name && p.PodUID == string(target.UID) }) {
		return block(errors.New("restart target changed before admission"))
	}
	m.Persistent = members
	if err := r.authorizeMaintenanceAction(ctx, w, m); err != nil {
		return block(err)
	}
	m.Phase = "Stopping"
	return save()
}

func (r *Reconciler) executePersistentStopping(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, res, j, m, save, block := p.f, p.res, p.j, p.m, p.save, p.block
	target := m.Targets[m.Index]
	index := slices.IndexFunc(m.Persistent, func(p persistentMember) bool { return p.PodUID == string(target.UID) })
	if index < 0 {
		return block(errors.New("restart donor capture missing"))
	}
	old := m.Persistent[index]
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: target.Name}, pod); err != nil {
		return block(err)
	}
	id, _ := podIdentity(pod)
	if pod.UID != target.UID || id != old.Container {
		return block(errors.New("restart donor identity changed"))
	}
	state, err := r.callLauncher(ctx, f, pod, "", "")
	if err != nil {
		return block(err)
	}
	if state.Invocation != old.Invocation || state.Generation != old.Generation {
		return block(errors.New("restart donor launcher changed before stop"))
	}
	if state.Phase == "Running" {
		if m.Deadline.IsZero() || !r.capacityNow().Before(m.Deadline) {
			return block(errors.New("restart deadline expired before child stop"))
		}
		members, _, err := r.assessPersistent(ctx, f, &p.view, false, false)
		if err != nil {
			return block(err)
		}
		for _, member := range members {
			if !slices.ContainsFunc(m.Persistent, func(prior persistentMember) bool { return sameInvocation(prior, member) }) {
				return block(errors.New("restart survivor changed before child stop"))
			}
		}
		m.Persistent = members
		p.view.Operation.PersistentMembers = members
		if err := r.saveJournal(ctx, res, j); err != nil {
			return ctrl.Result{}, true, err
		}
		stopCtx, cancel := context.WithDeadline(ctx, m.Deadline)
		state, err = r.callLauncher(stopCtx, f, pod, m.ID, old.Generation)
		cancel()
		if err != nil {
			return block(err)
		}
	}
	if state.Operation != m.ID || state.Invocation != old.Invocation || state.Generation != old.Generation {
		return block(errors.New("restart receipt changed invocation"))
	}
	if !state.RemovalReady() {
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	members, _, err := r.assessPersistent(ctx, f, &p.view, true, false)
	if err != nil {
		return block(err)
	}
	m.Persistent = members
	for _, member := range members {
		if member.Node == target.Name {
			member.Retired = true
			member.Stopped = true
			upsertMaintenanceMember(j, member)
		}
	}
	m.Phase = "Authorized"
	return save()
}

func (r *Reconciler) executePersistentAuthorized(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, m, save, block := p.f, p.m, p.save, p.block
	target := m.Targets[m.Index]
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: target.Name}, pod)
	if err != nil && !apierrors.IsNotFound(err) {
		return block(err)
	}
	if err == nil && pod.UID == target.UID {
		if err := r.Delete(ctx, pod, client.Preconditions{UID: &target.UID}); err != nil && !apierrors.IsNotFound(err) {
			return block(err)
		}
	}
	m.Phase = "Recovering"
	return save()
}

func (r *Reconciler) executePersistentRecovering(ctx context.Context, p *maintenancePass) (ctrl.Result, bool, error) {
	f, res, j, m, save, block := p.f, p.res, p.j, p.m, p.save, p.block
	if err := r.schedulePersistent(ctx, f, res, j); err != nil {
		return block(err)
	}
	members, err := r.persistentMembers(ctx, f, &p.view, j.Applied, false)
	if err != nil {
		return block(err)
	}
	target := m.Targets[m.Index]
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return block(err)
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return block(err)
	}
	inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
	if err != nil {
		return block(err)
	}
	for _, member := range members {
		priorIndex := slices.IndexFunc(m.Persistent, func(p persistentMember) bool { return p.Node == member.Node })
		if priorIndex < 0 {
			return block(errors.New("replacement is outside admitted fleet"))
		}
		prior := m.Persistent[priorIndex]
		if member.Node == target.Name {
			if member.PodUID == prior.PodUID || member.Generation == prior.Generation || member.ClaimUID != prior.ClaimUID || member.VolumeUID != prior.VolumeUID || member.VolumeHandle != prior.VolumeHandle {
				return block(errors.New("restart replacement lacks retained volume continuity"))
			}
			if member.Host != prior.Host || member.HostUID != prior.HostUID || member.BootID != prior.BootID {
				if prior.DiskID == "" || prior.DiskID != member.DiskID || prior.Zone == "" || prior.Zone != member.Zone || r.Options.LocalTest {
					return block(errors.New("cross-host restart lacks authenticated disk and zone continuity"))
				}
				if err := r.verifyVolumeAttachment(ctx, f, member); err != nil {
					return block(err)
				}
			}
		} else if !sameInvocation(prior, member) {
			return block(errors.New("survivor changed during restart"))
		}
		if !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool {
			return n.Name == member.Node && n.Generation == member.Generation && n.ExpiresMS > uint64(r.capacityNow().UnixMilli())
		}) {
			return block(errors.New("replacement membership lacks live storage evidence"))
		}
	}
	if t := awaitSettling(&m.SettledAt, inventory.ObservedAt, save, requeueSoon); t != nil {
		return t.unwrap()
	}
	for _, member := range members {
		upsertMaintenanceMember(j, member)
	}
	m.Index++
	m.Phase = "Next"
	m.SettledAt = time.Time{}
	return save()
}

func upsertMaintenanceMember(j *lifecycleJournal, m persistentMember) {
	i := slices.IndexFunc(j.PersistentHistory, func(p persistentMember) bool { return p.Node == m.Node && p.Generation == m.Generation })
	if i < 0 {
		j.PersistentHistory = append(j.PersistentHistory, m)
	} else {
		j.PersistentHistory[i] = m
	}
}

func (r *Reconciler) executePersistentShutdown(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object, block func(error) (ctrl.Result, bool, error)) (ctrl.Result, bool, error) {
	m := j.Maintenance
	save := func() (ctrl.Result, bool, error) {
		return r.saveMaintenance(ctx, f, res, j)
	}
	if f.DeletionTimestamp.IsZero() && !m.Coordinated {
		return block(errors.New("shutdown requires deleting fleet"))
	}
	switch m.Phase {
	case "Capture":
		view := *j
		view.Operation = &lifecycleOperation{ID: m.ID, From: j.Applied, To: j.Applied}
		members, err := r.persistentMembers(ctx, f, &view, j.Applied, false)
		if err != nil {
			return block(err)
		}
		inventory, err := r.shutdownInventory(ctx, f, j, members, false)
		if err != nil {
			return block(err)
		}
		for i := range members {
			for _, n := range inventory.Nodes {
				if n.Name == members[i].Node {
					members[i].Epoch = n.Epoch
					members[i].Ensemble = slices.Clone(n.Ensemble)
				}
			}
		}
		m.Persistent = members
		if m.Coordinated {
			if err := r.authorizeMaintenanceAction(ctx, w, m); err != nil {
				return block(err)
			}
		}
		m.Phase = "Stopping"
		return save()
	case "Stopping":
		for i := range m.Persistent {
			old := &m.Persistent[i]
			pod := &corev1.Pod{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: old.Node}, pod); err != nil {
				return block(err)
			}
			id, _ := podIdentity(pod)
			if string(pod.UID) != old.PodUID || id != old.Container {
				return block(errors.New("shutdown invocation changed"))
			}
			state, err := r.callLauncher(ctx, f, pod, "", "")
			if err != nil {
				return block(err)
			}
			if state.Phase == "Running" {
				stopCtx, cancel := context.WithDeadline(ctx, m.Deadline)
				state, err = r.callLauncher(stopCtx, f, pod, m.ID, old.Generation)
				cancel()
				if err != nil {
					return block(err)
				}
			}
			if state.Operation != m.ID || state.Invocation != old.Invocation || state.Generation != old.Generation {
				return block(errors.New("shutdown receipt differs from exact invocation"))
			}
			if !state.RemovalReady() {
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			if !old.Stopped {
				old.RestartDenied = true
				old.Stopped = true
				return save()
			}
		}
		if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
			return block(err)
		}
		if m.Coordinated {
			m.Phase = "Quiesced"
		} else {
			m.Phase = "Authorized"
		}
		return save()
	case "Authorized":
		if replicas(w) != 0 {
			if err := r.verifyShutdownStops(ctx, f, m); err != nil {
				return block(err)
			}
		}
		if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
			return block(err)
		}
		admit := func() *transition {
			if replicas(w) != j.Applied || w.GetAnnotations()[maintenanceFenceKey] != "deleting" {
				return asTransition(block(errors.New("shutdown workload authority changed")))
			}
			return nil
		}
		unauthorized := func() *transition {
			return asTransition(block(errors.New("missing shutdown replica authority")))
		}
		if t := r.scaleToZero(ctx, w, m.ID, admit, unauthorized); t != nil {
			return t.unwrap()
		}
		m.Phase = "Recovering"
		return save()
	case "Recovering":
		pods, err := r.Evidence.pods(ctx, f)
		if err != nil {
			return block(err)
		}
		if len(pods) != 0 {
			return block(errors.New("waiting for stopped workload membership cleanup"))
		}
		evidence, err := r.shutdownInventory(ctx, f, j, m.Persistent, true)
		if err != nil {
			return block(err)
		}
		if t := awaitSettling(&m.SettledAt, evidence.ObservedAt, save, requeueSoon); t != nil {
			return t.unwrap()
		}
		for _, member := range m.Persistent {
			member.Retired = true
			upsertMaintenanceMember(j, member)
		}
		m.Phase = "Cleanup"
		return save()
	case "Cleanup":
		uid := w.GetUID()
		if err := r.Delete(ctx, w, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, err
		}
		return r.completeRetainedDeletion(ctx, f, res, j)
	default:
		return block(errors.New("invalid persistent shutdown phase"))
	}
}

func (r *Reconciler) shutdownInventory(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, members []persistentMember, stopped bool) (v050.Inventory, error) {
	if len(members) == 0 || len(members) != int(j.Applied) {
		return v050.Inventory{}, errors.New("shutdown capture does not cover the entire applied fleet")
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return v050.Inventory{}, err
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return v050.Inventory{}, err
	}
	inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
	if err != nil {
		return inventory, err
	}
	if err := observeShutdownEpochs(j, members, inventory); err != nil {
		return inventory, err
	}
	for _, session := range j.Inventory.Sessions {
		if !slices.ContainsFunc(members, func(m persistentMember) bool { return m.Node == session.Node && m.Generation == session.Generation }) && !slices.ContainsFunc(j.PersistentHistory, func(m persistentMember) bool {
			return m.Node == session.Node && m.Generation == session.Generation && resolvedMember(m)
		}) {
			return inventory, errors.New("unresolved historical shutdown session")
		}
	}
	for _, n := range inventory.Nodes {
		i := slices.IndexFunc(members, func(m persistentMember) bool { return m.Node == n.Name && m.Generation == n.Generation })
		if i < 0 {
			if !slices.ContainsFunc(j.PersistentHistory, func(m persistentMember) bool {
				return m.Node == n.Name && m.Generation == n.Generation && resolvedMember(m)
			}) {
				return inventory, errors.New("unknown shutdown storage writer")
			}
			if n.ExpiresMS > uint64(r.capacityNow().UnixMilli()) || (n.LogState != "" && n.LogState != "sealed") {
				return inventory, errors.New("retired shutdown writer revived")
			}
			continue
		}
		m := members[i]
		if n.Epoch < m.Epoch {
			return inventory, errors.New("shutdown log epoch rewound")
		}
		if stopped {
			noOwnLog := n.Epoch == 0 && m.Epoch == 0 && n.LogState == ""
			if noOwnLog {
				for _, prior := range j.Inventory.Sessions {
					if prior.Node == n.Name && prior.Generation == n.Generation && prior.Epoch > 0 {
						noOwnLog = false
					}
				}
				for _, prior := range j.PersistentHistory {
					if prior.Node == n.Name && prior.Generation == n.Generation && prior.Epoch > 0 {
						noOwnLog = false
					}
				}
			}
			if !m.Stopped || !m.RestartDenied || n.ExpiresMS > uint64(r.capacityNow().UnixMilli()) || (n.LogState != "sealed" && !noOwnLog) {
				return inventory, errors.New("all stopped writers must have sealed own logs and expired leases")
			}
		} else if n.ExpiresMS <= uint64(r.capacityNow().UnixMilli()) {
			return inventory, errors.New("shutdown writer lease unavailable")
		}
	}
	for _, m := range members {
		if !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool { return n.Name == m.Node && n.Generation == m.Generation }) {
			return inventory, errors.New("shutdown node metadata missing")
		}
	}
	return inventory, nil
}

// Re-read every exact stopped invocation immediately before the compute change;
// an unexpected container restart must not inherit an earlier stop receipt.
func (r *Reconciler) verifyShutdownStops(ctx context.Context, f *fleet.CelldFleet, m *maintenanceOperation) error {
	for _, old := range m.Persistent {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: old.Node}, pod); err != nil {
			return err
		}
		id, _ := podIdentity(pod)
		if string(pod.UID) != old.PodUID || id != old.Container {
			return errors.New("shutdown process replaced after stop receipt")
		}
		state, err := r.callLauncher(ctx, f, pod, "", "")
		if err != nil {
			return err
		}
		if !old.RestartDenied || !state.RemovalReady() || state.Invocation != old.Invocation || state.Generation != old.Generation || state.Operation != m.ID || state.DiskID != old.DiskID {
			return errors.New("shutdown stop authority no longer matches launcher")
		}
	}
	return nil
}

// A shutdown scan may observe epochs newer than the initial capture, including
// while a different member still prevents progress. Retain those observations
// before returning a blocker so retry/replay cannot accept an older sealed log.
func observeShutdownEpochs(j *lifecycleJournal, members []persistentMember, inventory v050.Inventory) error {
	groups := [][]persistentMember{members, j.PersistentHistory}
	if j.Maintenance != nil {
		groups = append(groups, j.Maintenance.Persistent)
	}
	if j.Operation != nil {
		groups = append(groups, j.Operation.PersistentMembers)
	}
	for _, node := range inventory.Nodes {
		highest := uint64(0)
		for _, session := range j.Inventory.Sessions {
			if session.Node == node.Name && session.Generation == node.Generation {
				highest = max(highest, session.Epoch)
			}
		}
		for _, group := range groups {
			for _, member := range group {
				if member.Node == node.Name && member.Generation == node.Generation {
					highest = max(highest, member.Epoch)
				}
			}
		}
		if node.Epoch < highest {
			return errors.New("shutdown log epoch rewound below previously observed generation")
		}
		if node.Epoch > highest {
			j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: node.Name, Generation: node.Generation, Epoch: node.Epoch, FirstSeen: inventory.ObservedAt, LastSeen: inventory.ObservedAt, Association: "ShutdownMetadataObservation"})
		}
		for _, group := range groups {
			for i := range group {
				if group[i].Node == node.Name && group[i].Generation == node.Generation {
					group[i].Epoch = node.Epoch
				}
			}
		}
	}
	return nil
}
