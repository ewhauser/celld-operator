package controller

import (
	"context"
	"hash/fnv"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var fleetGauges = map[string]*prometheus.GaugeVec{}

func init() {
	for name, help := range map[string]string{
		"desired_replicas":          "Latest requested replica count (shadow policy recommendations are informational).",
		"applied_replicas":          "Replica target currently applied to the owned workload.",
		"observed_replicas":         "Pods observed for the fleet, including terminating pods.",
		"ready_replicas":            "Ready replicas reported by the owned workload.",
		"joining_replicas":          "Observed non-terminating pods that are not Ready.",
		"terminating_replicas":      "Observed pods with a deletion timestamp.",
		"replica_observation_valid": "One when the fleet Pod inventory is complete.",
		"blocked":                   "One when the latest reconcile reports a blocker.",
		"operation_stalled":         "One when the persisted lifecycle operation exceeded its deadline.",
		"operation_age_seconds":     "Age of the current durable operation, zero when none.",
		"blocked_age_seconds":       "Age of the current continuously reported blocker, zero when none.",
		"operation_bytes":           "Bounded current operation and policy state bytes, capped at 180 KiB.",
	} {
		gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "celld_fleet_" + name, Help: help}, []string{"namespace", "fleet"})
		metrics.Registry.MustRegister(gauge)
		fleetGauges[name] = gauge
	}
}

func clearFleetMetrics(namespace, name string) {
	for _, gauge := range fleetGauges {
		gauge.DeleteLabelValues(namespace, name)
	}
}
func secondsSince(value string, now time.Time) float64 {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil || at.After(now) {
		return 0
	}
	return now.Sub(at).Seconds()
}
func boolean(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
func publishFleetMetrics(f *fleet.CelldFleet, state stateFootprint, now time.Time) {
	values := map[string]float64{
		"operation_bytes":  float64(state.bytes),
		"desired_replicas": float64(f.Status.DesiredReplicas), "applied_replicas": float64(f.Status.AppliedReplicas),
		"observed_replicas": float64(f.Status.ObservedReplicas), "ready_replicas": float64(f.Status.ReadyReplicas),
		"joining_replicas": float64(f.Status.JoiningReplicas), "terminating_replicas": float64(f.Status.TerminatingReplicas),
		"replica_observation_valid": boolean(f.Status.ReplicaObservationValid),
		"blocked":                   boolean(meta.IsStatusConditionTrue(f.Status.Conditions, "Blocked")),
		"operation_stalled":         boolean(f.Status.Lifecycle.Stalled),
		"operation_age_seconds":     secondsSince(f.Status.Lifecycle.StartedAt, now),
		"blocked_age_seconds":       secondsSince(f.Status.BlockedSince, now),
	}
	for name, value := range values {
		fleetGauges[name].WithLabelValues(f.Namespace, f.Name).Set(value)
	}
}
func (r *Reconciler) observeReplicaCounts(ctx context.Context, f *fleet.CelldFleet) {
	f.Status.ObservedReplicas, f.Status.JoiningReplicas, f.Status.TerminatingReplicas = 0, 0, 0
	f.Status.ReplicaObservationValid = false
	// /scale contract: the selector matches exactly this fleet's pods.
	f.Status.LabelSelector = metav1.FormatLabelSelector(selector(f))
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f)), client.Limit(201)); err != nil || pods.Continue != "" {
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue // terminal pods are not capacity for an autoscaler
		}
		f.Status.ObservedReplicas++
		if !p.DeletionTimestamp.IsZero() {
			f.Status.TerminatingReplicas++
		} else if !podReady(p) {
			f.Status.JoiningReplicas++
		}
	}
	f.Status.ReplicaObservationValid = true
	f.Status.Replicas = f.Status.ObservedReplicas
}
func (r *Reconciler) recordConditionChange(f *fleet.CelldFleet, before []metav1.Condition) {
	if r.Recorder == nil {
		return
	}
	current := meta.FindStatusCondition(f.Status.Conditions, "Blocked")
	previous := meta.FindStatusCondition(before, "Blocked")
	if current == nil || (previous != nil && previous.Status == current.Status && previous.Reason == current.Reason) {
		return
	}
	kind := corev1.EventTypeNormal
	if current.Status == metav1.ConditionTrue {
		kind = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(f, nil, kind, current.Reason, "Reconcile", "%s", current.Message)
}
func reconcileDelay(f *fleet.CelldFleet) time.Duration {
	// Stable per-fleet jitter avoids synchronized polling without making policy
	// sample timestamps or safety deadlines nondeterministic.
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(f.UID))
	return 5*time.Second + time.Duration(hash.Sum32()%2000)*time.Millisecond
}
