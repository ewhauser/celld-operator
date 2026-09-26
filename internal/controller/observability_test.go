package controller

import (
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
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

// The warning fires at half of either budget that fails closed, and never one
// byte earlier.
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
