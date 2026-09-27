package controller

import (
	"context"
	"hash/fnv"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var fleetGauges = map[string]*prometheus.GaugeVec{}

// capacityGauges report the policy's latest decision in every mode, including
// Shadow, where it is not applied.
var capacityGauges = map[string]*prometheus.GaugeVec{}

// capacityDecision is one for the latest reason and zero for the rest of the
// fixed set; a reason missing from capacity.Reasons reports as Other.
var capacityDecision = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "celld_fleet_capacity_decision", Help: "One for the capacity policy's latest reason, zero for every other reason."}, []string{"namespace", "fleet", "reason"})

func init() {
	for name, help := range map[string]string{
		"desired_replicas":          "Replica count the fleet requests: spec.replicas, or the capacity policy's count in ScaleOut and Automatic modes. See celld_fleet_capacity_recommended_replicas for the policy's recommendation in every mode.",
		"applied_replicas":          "Replica target currently applied to the owned workload.",
		"observed_replicas":         "Pods observed for the fleet, including terminating pods.",
		"ready_replicas":            "Ready replicas reported by the owned workload.",
		"joining_replicas":          "Observed non-terminating pods that are not Ready.",
		"terminating_replicas":      "Observed pods with a deletion timestamp.",
		"replica_observation_valid": "One when the fleet Pod inventory is complete.",
		"blocked":                   "One when the latest reconcile reports a blocker.",
		"blocked_age_seconds":       "Age of the current continuously reported blocker, zero when none.",
		"operation_bytes":           "Bytes of the operator's bounded fleet state on the storage reservation.",
	} {
		gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "celld_fleet_" + name, Help: help}, []string{"namespace", "fleet"})
		metrics.Registry.MustRegister(gauge)
		fleetGauges[name] = gauge
	}
	for name, help := range map[string]string{
		"recommended_replicas": "Replica count the capacity policy recommends, whether or not its mode applies it.",
		"useful_replicas":      "Replicas the capacity policy observed as ready and useful.",
		"pending_replicas":     "Replicas requested but not yet observed as useful by the capacity policy.",
		"covered_replicas":     "Replicas covered by fresh capacity policy observations.",
	} {
		gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "celld_fleet_capacity_" + name, Help: help}, []string{"namespace", "fleet"})
		metrics.Registry.MustRegister(gauge)
		capacityGauges[name] = gauge
	}
	metrics.Registry.MustRegister(capacityDecision)
}

func clearFleetMetrics(namespace, name string) {
	for _, gauge := range fleetGauges {
		gauge.DeleteLabelValues(namespace, name)
	}
	clearCapacityMetrics(namespace, name)
}
func clearCapacityMetrics(namespace, name string) {
	for _, gauge := range capacityGauges {
		gauge.DeleteLabelValues(namespace, name)
	}
	capacityDecision.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "fleet": name})
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
		"blocked_age_seconds":       secondsSince(f.Status.BlockedSince, now),
	}
	for name, value := range values {
		fleetGauges[name].WithLabelValues(f.Namespace, f.Name).Set(value)
	}
	publishCapacityMetrics(f)
}

// publishCapacityMetrics reports status.capacity. A fleet without a policy
// result has no series: zeros would read as a recommendation of zero replicas.
func publishCapacityMetrics(f *fleet.CelldFleet) {
	c := f.Status.Capacity
	if f.Spec.Capacity == nil || externalOwner(f) || c.Reason == "" {
		clearCapacityMetrics(f.Namespace, f.Name)
		return
	}
	for name, value := range map[string]int32{
		"recommended_replicas": c.DesiredReplicas, "useful_replicas": c.UsefulReplicas,
		"pending_replicas": c.PendingReplicas, "covered_replicas": c.CoveredReplicas,
	} {
		capacityGauges[name].WithLabelValues(f.Namespace, f.Name).Set(float64(value))
	}
	current := "Other"
	if slices.Contains(capacity.Reasons, c.Reason) {
		current = c.Reason
	}
	for _, reason := range append(slices.Clone(capacity.Reasons), "Other") {
		capacityDecision.WithLabelValues(f.Namespace, f.Name, reason).Set(boolean(reason == current))
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
