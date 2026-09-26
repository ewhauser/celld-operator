package controller

import (
	"context"
	"fmt"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
)

// capacityTarget returns the replica count to apply given the workload's
// applied count, and whether it is an automatic step. Its history is kept in j
// and saved on the reservation. It never edits spec.replicas or workload
// replicas.
func (r *Reconciler) capacityTarget(ctx context.Context, f *fleet.CelldFleet, j *fleetState, applied int32) (int32, bool) {
	if f.Spec.Capacity == nil {
		// Preserve timing across disable/re-enable. Manual intent remains independent.
		if j.Capacity != nil {
			j.Capacity.Config = ""
			j.Capacity.LastManual = f.Spec.Replicas
			j.Capacity.ManualTarget = 0
		}
		return f.Spec.Replicas, false
	}
	if j.Capacity == nil {
		j.Capacity = &capacity.State{LastManual: f.Spec.Replicas, ManualTarget: f.Spec.Replicas}
	}
	s := j.Capacity
	if externalOwner(f) {
		// One external writer owns spec.replicas through /scale. The built-in
		// policy computes nothing, collects nothing, and never writes the field
		// back; lifecycle gates still decide whether a requested count is applied.
		s.LastManual, s.ManualTarget, s.Config = f.Spec.Replicas, 0, ""
		s.Decision = fleet.CapacityStatus{Mode: "External", Reason: "ExternalOwner", Message: fmt.Sprintf("spec.replicas is owned by the /scale writer: desired %d, applied %d", f.Spec.Replicas, applied), DesiredReplicas: f.Spec.Replicas}
		return f.Spec.Replicas, false
	}
	// Manual edits win one intent, even in automatic mode. Persist the new baseline
	// before observing again; an in-flight intent is never retargeted.
	if s.LastManual != f.Spec.Replicas {
		s.LastManual = f.Spec.Replicas
		s.ManualTarget = f.Spec.Replicas
		s.Config = ""
	}
	if s.ManualTarget == applied {
		s.ManualTarget = 0
	}
	if s.ManualTarget != 0 {
		s.Decision = fleet.CapacityStatus{Mode: f.Spec.Capacity.Mode, Reason: "ManualOverride", Message: "Manual replica edit takes precedence; stabilization restarts", DesiredReplicas: s.ManualTarget}
		return s.ManualTarget, false
	}
	observation := capacity.Observation{At: r.capacityNow()}
	if r.Collector != nil {
		observation = r.Collector.Collect(ctx, f)
	}
	*s = capacity.Evaluate(*f.Spec.Capacity, *s, observation, applied)
	target := s.Decision.DesiredReplicas
	if f.Spec.Capacity.Mode == "Shadow" || !s.Actionable {
		return applied, false
	}
	if target < applied && f.Spec.Capacity.Mode != "Automatic" {
		return applied, false
	}
	return target, target != applied
}
func (r *Reconciler) capacityNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// externalOwner reports whether spec.replicas belongs to an external /scale
// writer. Contractions it requests are automatic in effect and share the
// production release gate of the built-in Automatic mode.
func externalOwner(f *fleet.CelldFleet) bool {
	return f.Spec.Capacity != nil && f.Spec.Capacity.Mode == "External"
}
