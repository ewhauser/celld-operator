package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/catalog"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func coordinatedDowntime(f *fleet.CelldFleet) bool {
	return f.Spec.Profile == "PersistentFleet" && f.Spec.Maintenance != nil && f.Spec.Maintenance.AllowCoordinatedDowntime
}

func coordinatedExpectedReplicas(m *maintenanceOperation, applied int32, w client.Object) int32 {
	switch m.Phase {
	case "Empty":
		if w.GetAnnotations()[operationKey] == m.ID {
			return 0
		}
	case "Resuming":
		if w.GetAnnotations()[operationKey] == m.ID+"/resume" {
			return m.TargetReplicas
		}
		return 0
	}
	return applied
}

func (r *Reconciler) beginCoordinatedContraction(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	if !coordinatedDowntime(f) || paused(f) || !f.DeletionTimestamp.IsZero() || op == nil || op.Automatic || op.From != 2 || op.To != 1 || f.Spec.Replicas != 1 || f.Spec.Placement.AZCount > 1 || !r.capacityNow().Before(op.Deadline) || w.GetAnnotations()[maintenanceFenceKey] != "" {
		result, err := r.report(ctx, f, "CoordinatedDowntimeBlocked", "Manual 2-to-1 requires explicit downtime permission and a one-AZ floor", 0, false)
		return result, true, err
	}
	j.Maintenance = &maintenanceOperation{ID: op.ID, Kind: "Contract", Coordinated: true, TargetReplicas: 1, Phase: "Capture", StartedAt: op.StartedAt, Deadline: op.Deadline}
	j.Operation = nil
	return r.saveMaintenance(ctx, f, res, j)
}

// The all-sealed boundary is essential: this runtime can declare loss against
// expired, unreachable followers during startup. Retained disks alone do not
// authorize starting a partially available, unsealed fleet.
func (r *Reconciler) executeCoordinatedPersistent(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object, block func(error) (ctrl.Result, bool, error)) (ctrl.Result, bool, error) {
	m := j.Maintenance
	save := func() (ctrl.Result, bool, error) { return r.saveMaintenance(ctx, f, res, j) }
	if m.TargetReplicas < f.Spec.Placement.AZCount {
		return block(errors.New("coordinated target is below the configured AZ floor"))
	}
	switch m.Phase {
	case "Capture":
		if !coordinatedDowntime(f) {
			return block(errors.New("coordinated downtime requires explicit opt-in"))
		}
		return r.executePersistentShutdown(ctx, f, res, j, w, block)
	case "Stopping":
		return r.executePersistentShutdown(ctx, f, res, j, w, block)
	case "Quiesced":
		if err := r.verifyShutdownStops(ctx, f, m); err != nil {
			return block(err)
		}
		if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
			return block(err)
		}
		for _, member := range m.Persistent {
			member.Retired = true
			upsertMaintenanceMember(j, member)
		}
		m.Phase = "Empty"
		return save()
	case "Empty":
		if replicas(w) != 0 {
			if replicas(w) != j.Applied {
				return block(errors.New("coordinated workload replica authority changed"))
			}
			if err := r.verifyShutdownStops(ctx, f, m); err != nil {
				return block(err)
			}
			if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
				return block(err)
			}
			setReplicas(w, 0)
			w.GetAnnotations()[operationKey] = m.ID
			if err := r.Update(ctx, w); err != nil {
				return ctrl.Result{}, true, err
			}
			return save()
		}
		if w.GetAnnotations()[operationKey] != m.ID {
			return block(errors.New("missing coordinated zero-replica authority"))
		}
		pods, err := r.Evidence.pods(ctx, f)
		if err != nil {
			return block(err)
		}
		if len(pods) != 0 {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
			return block(err)
		}
		if m.Kind == "Upgrade" {
			if err := r.installStoppedRuntime(ctx, f, j, w); err != nil {
				return block(err)
			}
		}
		m.Phase = "Resuming"
		return save()
	case "Resuming":
		if replicas(w) == 0 {
			if w.GetAnnotations()[operationKey] != m.ID {
				return block(errors.New("missing coordinated resume authority"))
			}
			if _, err := r.shutdownInventory(ctx, f, j, m.Persistent, true); err != nil {
				return block(err)
			}
			setReplicas(w, m.TargetReplicas)
			w.GetAnnotations()[operationKey] = m.ID + "/resume"
			if err := r.Update(ctx, w); err != nil {
				return ctrl.Result{}, true, err
			}
			return save()
		}
		if replicas(w) != m.TargetReplicas || w.GetAnnotations()[operationKey] != m.ID+"/resume" {
			return block(errors.New("coordinated restored replicas differ"))
		}
		if err := r.schedulePersistent(ctx, f, j); err != nil {
			return block(err)
		}
		view := *j
		view.Operation = &lifecycleOperation{ID: m.ID, From: 0, To: m.TargetReplicas}
		members, err := r.persistentMembers(ctx, f, &view, m.TargetReplicas, false)
		if err != nil {
			return block(err)
		}
		reader, err := r.Evidence.reader(ctx, f)
		if err != nil {
			return block(err)
		}
		adapter, err := catalog.New(runtimeImage(f))
		if err != nil {
			return block(err)
		}
		inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
		if err != nil {
			return block(err)
		}
		for _, member := range members {
			i := slices.IndexFunc(m.Persistent, func(old persistentMember) bool { return old.Node == member.Node })
			if i < 0 {
				return block(errors.New("coordinated successor outside captured membership"))
			}
			old := m.Persistent[i]
			if member.PodUID == old.PodUID || member.Generation == old.Generation || member.ClaimUID != old.ClaimUID || member.VolumeUID != old.VolumeUID || member.VolumeHandle != old.VolumeHandle || member.DiskID != old.DiskID {
				return block(errors.New("coordinated successor lacks new invocation and retained disk continuity"))
			}
			if member.Host != old.Host || member.HostUID != old.HostUID || member.BootID != old.BootID {
				if old.DiskID == "" || old.Zone == "" || member.Zone != old.Zone || r.Options.LocalTest {
					return block(errors.New("coordinated successor host continuity unavailable"))
				}
				if err := r.verifyVolumeAttachment(ctx, f, member); err != nil {
					return block(err)
				}
			}
			if !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool {
				return n.Name == member.Node && n.Generation == member.Generation && n.ExpiresMS > uint64(r.capacityNow().UnixMilli())
			}) {
				return block(errors.New("coordinated successor has no live storage generation"))
			}
		}
		for _, node := range inventory.Nodes {
			if slices.ContainsFunc(members, func(member persistentMember) bool {
				return member.Node == node.Name && member.Generation == node.Generation
			}) {
				continue
			}
			if !slices.ContainsFunc(j.PersistentHistory, func(old persistentMember) bool {
				return old.Node == node.Name && old.Generation == node.Generation && old.Retired && old.Stopped && old.RestartDenied
			}) || node.ExpiresMS > uint64(r.capacityNow().UnixMilli()) || (node.LogState != "sealed" && (node.LogState != "" || node.Epoch != 0)) {
				return block(errors.New("unknown or revived writer during coordinated recovery"))
			}
		}
		// Recheck the exact processes after the remote evidence read.
		after, err := r.persistentMembers(ctx, f, &view, m.TargetReplicas, false)
		if err != nil {
			return block(err)
		}
		for _, member := range members {
			if !slices.ContainsFunc(after, func(current persistentMember) bool { return sameInvocation(member, current) }) {
				return block(errors.New("coordinated successor changed while collecting evidence"))
			}
		}
		if inventory.ObservedAt.After(r.capacityNow()) || r.capacityNow().Sub(inventory.ObservedAt) > 5*time.Second {
			return block(errors.New("coordinated evidence expired"))
		}
		if m.SettledAt.IsZero() {
			m.SettledAt = inventory.ObservedAt
			return save()
		}
		if inventory.ObservedAt.Sub(m.SettledAt) < 10*time.Second {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		for _, member := range members {
			upsertMaintenanceMember(j, member)
		}
		outcome := "CoordinatedContractionComplete"
		if m.Kind == "Restart" {
			j.CompletedRestarts = append(j.CompletedRestarts, m.Token)
			outcome = "CoordinatedRestartComplete"
		}
		if m.Kind == "Upgrade" {
			j.RuntimeImage = m.TargetImage
			outcome = "StoppedUpgradeComplete"
		}
		j.History = append(j.History, lifecycleCompletion{ID: m.ID, From: j.Applied, To: m.TargetReplicas, EvidenceAt: inventory.ObservedAt, Outcome: outcome})
		j.Applied = m.TargetReplicas
		j.Maintenance = nil
		j.Request = nil
		return save()
	default:
		return block(errors.New("invalid coordinated maintenance phase"))
	}
}
