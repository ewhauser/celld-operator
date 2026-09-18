package controller

import (
	"context"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
)

// capacityTarget runs inside the existing lifecycle authority after identity/drift
// checks. Its history and any new intent are committed together on the reservation.
// It never edits spec.replicas or workload replicas.
func (r *Reconciler) capacityTarget(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal) (int32, bool) {
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
	// Manual edits win one intent, even in automatic mode. Persist the new baseline
	// before observing again; an in-flight intent is never retargeted.
	if s.LastManual != f.Spec.Replicas {
		s.LastManual = f.Spec.Replicas
		s.ManualTarget = f.Spec.Replicas
		s.Config = ""
	}
	if s.ManualTarget == j.Applied {
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
	*s = capacity.Evaluate(*f.Spec.Capacity, *s, observation, j.Applied)
	target := s.Decision.DesiredReplicas
	if f.Spec.Capacity.Mode == "Shadow" || !s.Actionable {
		return j.Applied, false
	}
	if target < j.Applied && f.Spec.Capacity.Mode != "Automatic" {
		return j.Applied, false
	}
	return target, target != j.Applied
}
func (r *Reconciler) capacityNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
