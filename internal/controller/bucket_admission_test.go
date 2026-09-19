package controller

import (
	"context"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSteadyBucketAdmissionAllowsRestartBeforeFirstRemoval(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	j.Operation = nil
	j.Applied = 3
	j.Initial = 3
	j.Version = 7
	j.RuntimeImage = Image
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
	// Observation is allowed without Metrics Server or low demand: it has no
	// capacity effect and still verifies exact immutable posture and full S3.
	changed, err := r.admitBucketHistory(t.Context(), f, j)
	if err != nil || !changed || len(j.BucketHistory) != 3 {
		t.Fatalf("initial admission: %v %+v", err, j.BucketHistory)
	}
	res := &fleet.CelldStorageReservation{Name: reservationName(f)}
	if err := r.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	j, err = r.loadJournal(t.Context(), res)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-1"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].ContainerID = "successor"
	pod.Status.ContainerStatuses[0].RestartCount++
	if err := r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	id, _ := podIdentity(pod)
	next := j.Inventory.Sessions[1]
	j.Inventory.Sessions[1].Current = false
	next.Container = id
	next.Generation = "successor"
	next.Current = true
	j.Inventory.Sessions = append(j.Inventory.Sessions, next)
	reader.generation = map[string]string{"pod-1": "successor"}
	changed, err = r.admitBucketHistory(t.Context(), f, j)
	if err != nil || !changed || len(j.BucketHistory) != 4 {
		t.Fatalf("restart admission: %v %+v", err, j.BucketHistory)
	}
	found := false
	for _, session := range j.BucketHistory {
		if session.Node == "pod-1" && session.Generation == "generation" {
			found = session.Retired && session.SupersededBy == "successor"
		}
	}
	if !found {
		t.Fatal("known predecessor was discarded rather than superseded")
	}
	// The first real removal still executes the original capacity/health gates.
	j.Operation = &lifecycleOperation{ID: "first-removal", Phase: "Blocked", From: 3, To: 2}
	pods := &corev1.PodList{}
	if err := r.List(t.Context(), pods); err != nil {
		t.Fatal(err)
	}
	observation := capacity.Observation{At: reader.now, Complete: true}
	for i := range pods.Items {
		id, _ := podIdentity(&pods.Items[i])
		observation.Samples = append(observation.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
	}
	r.Collector = bucketCapacityCollector{observation}
	if _, _, err := r.bucketAssessment(t.Context(), f, j, 3, false); err != nil {
		t.Fatalf("first removal cannot use admitted restart lineage: %v", err)
	}
	observation.Samples[0].Pressured = true
	r.Collector = bucketCapacityCollector{observation}
	if _, _, err := r.bucketAssessment(t.Context(), f, j, 3, false); err == nil {
		t.Fatal("observational admission bypassed actual removal safety")
	}
}

func TestSteadyBucketAdmissionNeverAdoptsUnknownHistory(t *testing.T) {
	for _, which := range []string{"unknown-prior", "unknown-storage-writer", "stale-observation", "changed-posture", "unready"} {
		t.Run(which, func(t *testing.T) {
			p, f, j, opts, reader := bucketPreflightSetup(t)
			j.Operation = nil
			j.Applied = 3
			r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
			switch which {
			case "unknown-prior":
				previous := j.Inventory.Sessions[0]
				previous.Generation = "unknown-before-admission"
				previous.Current = false
				j.Inventory.Sessions = append(j.Inventory.Sessions, previous)
			case "unknown-storage-writer":
				reader.nodes = append(reader.nodes, "foreign")
			case "stale-observation":
				j.Inventory.CheckedAt = reader.now.Add(-time.Minute)
			case "changed-posture", "unready":
				pod := &corev1.Pod{}
				if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-1"}, pod); err != nil {
					t.Fatal(err)
				}
				if which == "changed-posture" {
					pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{Name: "override"}}}
					if err := r.Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				} else {
					pod.Status.Conditions = nil
					if err := r.Status().Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				}
			}
			changed, err := r.admitBucketHistory(t.Context(), f, j)
			if err == nil || changed || len(j.BucketHistory) != 0 {
				t.Fatalf("unqualified writer admitted: %v %+v", err, j.BucketHistory)
			}
		})
	}
}

func TestIncompleteSteadyAdmissionDoesNotBlockAddition(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	now := time.Unix(10000, 0)
	r.now = func() time.Time { return now }
	source := &inventoryReader{node: "unknown", gen: "generation", epoch: 1, now: now}
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	f = desiredCount(t, r, f, 4)
	for range 8 {
		reconcile(t, r, f)
	}
	j := getJournal(t, r, f)
	if j.Applied != 4 || j.Operation != nil || len(j.BucketHistory) != 0 {
		t.Fatalf("addition blocked or unknown history admitted: %+v", j)
	}
}

func TestSteadyBucketAdmissionInvalidatesRevivedExpiry(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	j.Operation = nil
	j.Applied = 3
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
	if _, err := r.admitBucketHistory(t.Context(), f, j); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-1"}, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	j.Applied = 2
	j.Inventory.Sessions[1].Current = false
	reader.expired = map[string]bool{"pod-1": true}
	if _, err := r.admitBucketHistory(t.Context(), f, j); err != nil {
		t.Fatal(err)
	}
	reader.expired["pod-1"] = false
	changed, err := r.admitBucketHistory(t.Context(), f, j)
	if err == nil || !changed {
		t.Fatal("revived retired lease did not invalidate prior admission")
	}
	for _, s := range j.BucketHistory {
		if s.Node == "pod-1" && (!s.ExpiryInvalidated || s.ExpiryObserved) {
			t.Fatal("old expiry authority remained valid")
		}
	}
}
