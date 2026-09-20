package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	publishFleetMetrics(f, journalFootprint{}, now)
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
	publishFleetMetrics(f, journalFootprint{}, now)
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
func TestJournalSizeThreshold(t *testing.T) {
	for _, m := range []journalFootprint{{bytes: archiveMaxBytes / 2}, {index: archiveIndexBytes / 2}, {}} {
		if m.nearCapacity() {
			t.Fatalf("warned within budget: %+v", m)
		}
	}
	for _, m := range []journalFootprint{{bytes: archiveMaxBytes/2 + 1}, {index: archiveIndexBytes/2 + 1}} {
		if !m.nearCapacity() {
			t.Fatalf("no warning past half the budget: %+v", m)
		}
	}
}

// A journal over half the 16 MiB hydrated cap raises JournalSizeWarning and is
// measured on the gauges, and neither blocks the reconcile.
func TestJournalSizeWarningAndGauges(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	t.Cleanup(func() { clearFleetMetrics(f.Namespace, f.Name) })
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	got := reconcile(t, r, f)
	if meta.IsStatusConditionTrue(got.Status.Conditions, "JournalSizeWarning") {
		t.Fatal("small journal warned")
	}
	inline := measureJournal(res)
	if inline.bytes == 0 || inline.pages != 0 {
		t.Fatalf("inline footprint: %+v", inline)
	}
	if bytes := testutil.ToFloat64(fleetGauges["journal_bytes"].WithLabelValues(f.Namespace, f.Name)); bytes != float64(inline.bytes) {
		t.Fatalf("journal_bytes=%v, want %d", bytes, inline.bytes)
	}
	if pages := testutil.ToFloat64(fleetGauges["journal_archive_pages"].WithLabelValues(f.Namespace, f.Name)); pages != 0 {
		t.Fatalf("journal_archive_pages=%v while inline", pages)
	}
	// Archive pages are bound to the reservation UID, which the fake client
	// does not assign on create.
	res.UID = "reservation-uid"
	if err := r.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	j, err := r.loadJournal(t.Context(), res)
	if err != nil {
		t.Fatal(err)
	}
	j.CompletedRestarts = append(j.CompletedRestarts, strings.Repeat("t", archiveMaxBytes/2+1024))
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	got = reconcile(t, r, f)
	warning := meta.FindStatusCondition(got.Status.Conditions, "JournalSizeWarning")
	if warning == nil || warning.Status != metav1.ConditionTrue || warning.Reason != "JournalNearCapacity" {
		t.Fatalf("no warning for an oversized journal: %+v", warning)
	}
	paged := measureJournal(res)
	if !strings.Contains(warning.Message, fmt.Sprint(paged.bytes)) || !strings.Contains(warning.Message, fmt.Sprint(archiveMaxBytes)) {
		t.Fatalf("message %q states neither the size nor the cap", warning.Message)
	}
	if paged.bytes <= archiveMaxBytes/2 || paged.pages == 0 {
		t.Fatalf("paged footprint: %+v", paged)
	}
	if bytes := testutil.ToFloat64(fleetGauges["journal_bytes"].WithLabelValues(f.Namespace, f.Name)); bytes != float64(paged.bytes) {
		t.Fatalf("journal_bytes=%v, want %d", bytes, paged.bytes)
	}
	if pages := testutil.ToFloat64(fleetGauges["journal_archive_pages"].WithLabelValues(f.Namespace, f.Name)); pages != float64(paged.pages) {
		t.Fatalf("journal_archive_pages=%v, want %d", pages, paged.pages)
	}
	// The warning informs; it never blocks.
	if meta.IsStatusConditionTrue(got.Status.Conditions, "Blocked") {
		t.Fatal("journal size blocked the reconcile")
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
			j := getJournal(t, r, f)
			j.Capacity = &capacity.State{Decision: fleet.CapacityStatus{DesiredReplicas: 5}}
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			if err := r.saveJournal(t.Context(), res, j); err != nil {
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
