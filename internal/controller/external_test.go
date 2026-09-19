package controller

import (
	"context"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type forbiddenCollector struct{ t *testing.T }

func (c forbiddenCollector) Collect(context.Context, *fleet.CelldFleet) capacity.Observation {
	c.t.Fatal("External mode must not collect load samples")
	return capacity.Observation{}
}

// In External mode the built-in policy computes nothing: the /scale writer's
// spec.replicas is the target, additions execute through the manual path, and
// the decision explains who owns the count.
func TestExternalModeDelegatesDesiredCountToScaleWriter(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			r.Collector = forbiddenCollector{t}
			f = enableCapacity(t, r, f, "External")
			got := reconcile(t, r, f)
			if got.Status.Capacity.Mode != "External" || got.Status.Capacity.Reason != "ExternalOwner" || got.Status.Capacity.DesiredReplicas != 3 {
				t.Fatalf("external decision not projected: %+v", got.Status.Capacity)
			}
			// An HPA writes through /scale, which lands in spec.replicas.
			f = desiredCount(t, r, f, 5)
			for range 8 {
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, Collector: forbiddenCollector{t}}
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Applied != 5 || j.Operation != nil {
				t.Fatalf("external addition not applied: %+v", j)
			}
			for _, h := range j.History {
				if h.ID != "" && j.Capacity != nil && j.Capacity.Config != "" {
					t.Fatal("external mode must not record a policy configuration")
				}
			}
			got = reconcile(t, r, f)
			if got.Spec.Replicas != 5 {
				t.Fatal("operator wrote spec.replicas back; it must never fight the external writer")
			}
			if got.Status.Capacity.DesiredReplicas != 5 || got.Status.DesiredReplicas != 5 || got.Status.AppliedReplicas != 5 {
				t.Fatalf("desired/applied not published: %+v %+v", got.Status.Capacity, got.Status)
			}
		})
	}
}

// Contraction requested by the external writer shares the production release
// gate with Automatic mode; additions are unaffected.
func TestExternalContractionIsReleaseGatedInProduction(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Replicas = 2
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "External"}
	f.Default()
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	j.Version = 8
	j.RuntimeImage = Image
	j.Initial, j.Applied = 3, 3
	j.Operation.StartedAt = reader.now
	j.Operation.Deadline = reader.now.Add(operationBudget)
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	if err := p.client.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	w := workload(f, opts)
	w.SetUID(j.WorkloadUID)
	setReplicas(w, 3)
	if err := p.client.Create(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.contractBucket(t.Context(), f, res, j, w); err != nil {
		t.Fatal(err)
	}
	if got := readyReason(t, r, f); got != "ExternalContractionUnqualified" {
		t.Fatalf("production external contraction not gated: %s", got)
	}
	if replicas(w) != 3 {
		t.Fatal("replica effect issued despite the gate")
	}
	// The disposable local fixture runs the same executor.
	r.Options.LocalTest = true
	f.Spec.Capacity = nil
	if _, _, err := r.contractBucket(t.Context(), f, res, j, w); err != nil {
		t.Fatal(err)
	}
	if got := readyReason(t, r, f); got == "ExternalContractionUnqualified" {
		t.Fatal("gate applied without an external owner")
	}
}

// status.replicas and status.labelSelector implement the /scale contract:
// non-terminal pods including terminating ones, and a selector for exactly
// this fleet's pods.
func TestScaleStatusCountsNonTerminalPods(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	now := metav1.Now()
	pods := []client.Object{
		&corev1.Pod{Name: "a", Namespace: f.Namespace, UID: types.UID("a"), Labels: labels(f), Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{Name: "b", Namespace: f.Namespace, UID: types.UID("b"), Labels: labels(f), DeletionTimestamp: &now, Finalizers: []string{"test"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&corev1.Pod{Name: "c", Namespace: f.Namespace, UID: types.UID("c"), Labels: labels(f), Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		&corev1.Pod{Name: "other", Namespace: f.Namespace, UID: types.UID("o"), Labels: map[string]string{FleetLabel: "someone-else"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	}
	r := setup(t, append(pods, f)...)
	r.observeReplicaCounts(t.Context(), f)
	if f.Status.Replicas != 2 || f.Status.TerminatingReplicas != 1 {
		t.Fatalf("scale status wrong: replicas=%d terminating=%d", f.Status.Replicas, f.Status.TerminatingReplicas)
	}
	if f.Status.LabelSelector != FleetLabel+"="+string(f.UID) {
		t.Fatalf("label selector %q", f.Status.LabelSelector)
	}
}
