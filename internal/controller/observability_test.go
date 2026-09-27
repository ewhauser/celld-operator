package controller

import (
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestFleetObservabilityTransitions(t *testing.T) {
	f := fixture("telemetry", "telemetry", "Bucket")
	now := time.Now().UTC().Truncate(time.Second)
	f.Status.DesiredReplicas = 3
	f.Status.AppliedReplicas = 2
	f.Status.BlockedSince = now.Add(-time.Minute).Format(time.RFC3339)
	f.Status.Conditions = []metav1.Condition{{Type: "Blocked", Status: metav1.ConditionTrue, Reason: "EvidenceUnavailable", Message: "waiting"}}
	publishFleetMetrics(f, stateFootprint{}, now)
	t.Cleanup(func() { clearFleetMetrics(f.Namespace, f.Name) })
	if got := testutil.ToFloat64(fleetGauges["blocked_age_seconds"].WithLabelValues(f.Namespace, f.Name)); got != 60 {
		t.Fatalf("age=%v", got)
	}
	recorder := events.NewFakeRecorder(4)
	r := setup(t)
	r.Recorder = recorder
	r.recordConditionChange(f, nil)
	r.recordConditionChange(f, f.Status.Conditions)
	if len(recorder.Events) != 1 {
		t.Fatalf("duplicate condition events: %d", len(recorder.Events))
	}
	prior := append([]metav1.Condition(nil), f.Status.Conditions...)
	f.Status.BlockedSince = ""
	f.Status.Conditions[0].Status = metav1.ConditionFalse
	f.Status.Conditions[0].Reason = "Ready"
	r.recordConditionChange(f, prior)
	publishFleetMetrics(f, stateFootprint{}, now)
	if len(recorder.Events) != 2 || testutil.ToFloat64(fleetGauges["blocked"].WithLabelValues(f.Namespace, f.Name)) != 0 {
		t.Fatal("unblock transition missing")
	}
	clearFleetMetrics(f.Namespace, f.Name)
	if fleetGauges["blocked"].DeleteLabelValues(f.Namespace, f.Name) {
		t.Fatal("deleted fleet metrics retained")
	}
}

func TestObservedReplicaInventory(t *testing.T) {
	f := fixture("inventory", "inventory", "Bucket")
	at := metav1.Now()
	pods := []*corev1.Pod{
		{Name: "ready", Namespace: f.Namespace, Labels: labels(f), Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		{Name: "joining", Namespace: f.Namespace, Labels: labels(f)},
		{Name: "terminating", Namespace: f.Namespace, Labels: labels(f), DeletionTimestamp: &at, Finalizers: []string{"test"}},
	}
	r := setup(t, pods[0], pods[1], pods[2])
	r.observeReplicaCounts(t.Context(), f)
	if !f.Status.ReplicaObservationValid || f.Status.ObservedReplicas != 3 || f.Status.JoiningReplicas != 1 || f.Status.TerminatingReplicas != 1 {
		t.Fatalf("inventory: %+v", f.Status)
	}
	if delay := reconcileDelay(f); delay < 5*time.Second || delay >= 7*time.Second || delay != reconcileDelay(f) {
		t.Fatalf("jitter=%s", delay)
	}
}

func TestPolicyDesiredReplicaReporting(t *testing.T) {
	for _, mode := range []string{"Automatic", "ScaleOut", "Shadow"} {
		t.Run(mode, func(t *testing.T) {
			r, f := lifecycleSetup(t, "Bucket")
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
				t.Fatal(err)
			}
			f.Spec.Capacity = &fleet.CapacityPolicy{Mode: mode}
			j := &fleetState{Version: 1, FleetUID: f.UID, Capacity: &capacity.State{Decision: fleet.CapacityStatus{DesiredReplicas: 5}}}
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			if err := r.saveState(t.Context(), res, j); err != nil {
				t.Fatal(err)
			}
			if _, err := r.report(t.Context(), f, nil, "Ready", "test", true); err != nil {
				t.Fatal(err)
			}
			want := int32(5)
			if mode == "Shadow" {
				want = f.Spec.Replicas
			}
			if f.Status.DesiredReplicas != want {
				t.Fatalf("desired %d, want %d", f.Status.DesiredReplicas, want)
			}
		})
	}
}

// takeCapacitySeries deletes a fleet's capacity series and reports whether any
// existed, so it can assert absence without creating a series.
func takeCapacitySeries(namespace, name string) bool {
	present := capacityDecision.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "fleet": name}) > 0
	for _, gauge := range capacityGauges {
		present = gauge.DeleteLabelValues(namespace, name) || present
	}
	return present
}

func TestShadowRecommendationMetrics(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearFleetMetrics(f.Namespace, f.Name) })
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Shadow"}
	// report patches status and reloads the fleet, so the policy must be stored.
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	recommended := f.Spec.Replicas + 2
	j := &fleetState{Version: 1, FleetUID: f.UID, Capacity: &capacity.State{Decision: fleet.CapacityStatus{Mode: "Shadow", Reason: "ScaleOutRecommended", DesiredReplicas: recommended, UsefulReplicas: f.Spec.Replicas, CoveredReplicas: f.Spec.Replicas}}}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveState(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	if _, err := r.report(t.Context(), f, nil, "Ready", "test", true); err != nil {
		t.Fatal(err)
	}
	gauge := func(vec *prometheus.GaugeVec) float64 {
		return testutil.ToFloat64(vec.WithLabelValues(f.Namespace, f.Name))
	}
	if got := gauge(capacityGauges["recommended_replicas"]); got != float64(recommended) || got <= gauge(fleetGauges["applied_replicas"]) {
		t.Fatalf("recommended %v, applied %v", got, gauge(fleetGauges["applied_replicas"]))
	}
	if got := gauge(fleetGauges["desired_replicas"]); got != float64(f.Spec.Replicas) {
		t.Fatalf("shadow desired %v, want spec.replicas %d", got, f.Spec.Replicas)
	}
	if gauge(capacityGauges["covered_replicas"]) != float64(f.Spec.Replicas) || gauge(capacityGauges["pending_replicas"]) != 0 {
		t.Fatal("observation counts not published")
	}
}

func TestCapacityDecisionReasonSeries(t *testing.T) {
	f := fixture("decision", "decision", "Bucket")
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Shadow"}
	t.Cleanup(func() { clearFleetMetrics(f.Namespace, f.Name) })
	for _, step := range []struct{ reason, want string }{{"StabilizingOut", "StabilizingOut"}, {"ScaleOutRecommended", "ScaleOutRecommended"}, {"SomethingNew", "Other"}} {
		f.Status.Capacity = fleet.CapacityStatus{Mode: "Shadow", Reason: step.reason, DesiredReplicas: 4}
		publishFleetMetrics(f, stateFootprint{}, time.Now())
		var set []string
		for _, reason := range append(slices.Clone(capacity.Reasons), "Other") {
			if testutil.ToFloat64(capacityDecision.WithLabelValues(f.Namespace, f.Name, reason)) == 1 {
				set = append(set, reason)
			}
		}
		if len(set) != 1 || set[0] != step.want {
			t.Fatalf("reason %s: series at one %v, want [%s]", step.reason, set, step.want)
		}
		if n := testutil.CollectAndCount(capacityDecision); n < len(capacity.Reasons)+1 {
			t.Fatalf("decision series %d", n)
		}
	}
}

func TestCapacityMetricsRemoved(t *testing.T) {
	f := fixture("unpolicied", "unpolicied", "Bucket")
	t.Cleanup(func() { clearFleetMetrics(f.Namespace, f.Name) })
	decided := func() {
		f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Shadow"}
		f.Status.Capacity = fleet.CapacityStatus{Mode: "Shadow", Reason: "WithinThresholds", DesiredReplicas: 3}
		publishFleetMetrics(f, stateFootprint{}, time.Now())
	}
	for name, change := range map[string]func(){
		"policy removed":  func() { f.Spec.Capacity = nil },
		"external owner":  func() { f.Spec.Capacity.Mode = "External"; f.Status.Capacity.Reason = "ExternalOwner" },
		"no decision yet": func() { f.Status.Capacity = fleet.CapacityStatus{} },
	} {
		decided()
		change()
		publishFleetMetrics(f, stateFootprint{}, time.Now())
		if takeCapacitySeries(f.Namespace, f.Name) {
			t.Errorf("%s: capacity series retained", name)
		}
	}
	decided()
	r := setup(t)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
		t.Fatal(err)
	}
	if takeCapacitySeries(f.Namespace, f.Name) {
		t.Error("deleted fleet: capacity series retained")
	}
}
