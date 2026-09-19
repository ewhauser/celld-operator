package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBucketManualContractionCrashReplayAndRenewingOldLease(t *testing.T) {
	for _, contrary := range []string{"renewal", "replacement"} {
		t.Run(contrary, func(t *testing.T) { testBucketManualContractionCrashReplay(t, contrary) })
	}
}

func testBucketManualContractionCrashReplay(t *testing.T, contrary string) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Replicas = 2
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	j.Version = 5
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Operation.StartedAt = reader.now
	j.Operation.Deadline = reader.now.Add(operationBudget)
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	if err := p.client.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	w := workload(f, opts).(*appsv1.Deployment)
	w.UID = j.WorkloadUID
	setReplicas(w, 3)
	if err := p.client.Create(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	step := func() {
		t.Helper()
		// Reconstruct all authority from API objects at every boundary.
		if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
			t.Fatal(err)
		}
		var err error
		j, err = readJournal(res)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
			t.Fatal(err)
		}
		observation := capacity.Observation{At: reader.now, Complete: true}
		pods := &corev1.PodList{}
		if err := p.client.List(t.Context(), pods); err != nil {
			t.Fatal(err)
		}
		for i := range pods.Items {
			id, _ := podIdentity(&pods.Items[i])
			observation.Samples = append(observation.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
		}
		r = &Reconciler{Client: p.client, Evidence: p, Options: opts, Collector: bucketCapacityCollector{observation}, now: p.now}
		if _, _, err := r.contractBucket(t.Context(), f, res, j, w); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if j.Operation.Phase != "Intent" {
		t.Fatal("capture did not persist intent")
	}
	// Crash after the workload CAS but before persisting Recovering. The next
	// controller must reconstruct this one effect, not decrement again.
	if err := r.applyReplicas(t.Context(), w, j.Operation); err != nil {
		t.Fatal(err)
	}
	step()
	if j.Operation.Phase != "Recovering" || replicas(w) != 2 {
		t.Fatal("replica effect not issued")
	}
	pod := &corev1.Pod{}
	if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-2"}, pod); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	// Pod absence alone cannot complete: the old process still renews S3.
	for range 3 {
		step()
		reader.now = reader.now.Add(time.Second)
	}
	if j.Operation == nil || !j.Operation.SettledAt.IsZero() {
		t.Fatal("live removed lease accepted")
	}
	// A Retired bit from admission is not post-issue expiry authority.
	for i := range j.Operation.BucketCandidates {
		if j.Operation.BucketCandidates[i].Node == "pod-2" {
			j.Operation.BucketCandidates[i].Retired = true
		}
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	// GC before any positive expiry observation remains unresolved.
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-2" })
	step()
	if j.Operation == nil || !j.Operation.SettledAt.IsZero() {
		t.Fatal("missing unobserved lease became recovery proof")
	}
	reader.nodes = append(reader.nodes, "pod-2")
	reader.expired = map[string]bool{"pod-2": true}
	step() // durably capture exact generation's positive expiry
	// The pinned runtime may delete the record immediately after that read.
	// Reconstructing the controller must retain the observed fact, while still
	// requiring the complete survivor and log scan throughout settling.
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-2" })
	step()
	if j.Operation.SettledAt.IsZero() {
		t.Fatal("no settling after lease expiry")
	}
	// A lease renewal during settling revokes this observation, without claiming
	// the prior absence or expiry ever proved physical termination.
	reader.nodes = append(reader.nodes, "pod-2")
	reader.expired["pod-2"] = false
	if contrary == "replacement" {
		reader.generation = map[string]string{"pod-2": "replacement-generation"}
	}
	step()
	if !j.Operation.SettledAt.IsZero() {
		t.Fatal("uncertainty did not reset settling")
	}
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-2" })
	step()
	if !j.Operation.SettledAt.IsZero() {
		t.Fatal("renewal then disappearance reused superseded expiry proof")
	}
	reader.nodes = append(reader.nodes, "pod-2")
	reader.expired["pod-2"] = true
	reader.generation = nil
	step()
	step()
	reader.now = reader.now.Add(11 * time.Second)
	step()
	if j.Operation != nil || j.Applied != 2 || len(j.BucketHistory) != 3 || j.History[0].Outcome != "BucketMembershipConvergedProcessLivenessUnknown" {
		t.Fatalf("bad completion: %+v", j)
	}
	if !j.BucketHistory[2].Retired {
		t.Fatal("retired generation lost")
	}
	// Model a completed ordinary addition with a new Pod UID. Historical Bucket
	// sessions can be GCed only after durable completion, without blocking a
	// second removal. Retain their generation in the operator journal.
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-2" })
	survivor := &corev1.Pod{}
	if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-1"}, survivor); err != nil {
		t.Fatal(err)
	}
	added := survivor.DeepCopy()
	added.Name = "pod-3"
	added.Spec.NodeName = "host-pod-3"
	if err := p.client.Create(t.Context(), &corev1.Node{Name: added.Spec.NodeName, UID: "node-pod-3", Labels: map[string]string{corev1.LabelHostname: added.Spec.NodeName, corev1.LabelTopologyZone: "us-east-1a"}}); err != nil {
		t.Fatal(err)
	}
	added.UID = "pod-3"
	added.ResourceVersion = ""
	if err := p.client.Create(t.Context(), added); err != nil {
		t.Fatal(err)
	}
	id, _ := podIdentity(added)
	j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: "pod-3", Pod: "pod-3", PodUID: "pod-3", Container: id, Generation: "generation", Current: true})
	reader.nodes = append(reader.nodes, "pod-3")
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	setReplicas(w, 3)
	w.Annotations[operationKey] = "addition"
	if err := p.client.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	j.Applied = 3
	j.Operation = &lifecycleOperation{ID: "second", Phase: "Blocked", From: 3, To: 2, StartedAt: reader.now, Deadline: reader.now.Add(operationBudget)}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	step()
	step()
	if j.Operation.Phase != "Recovering" {
		t.Fatalf("historical session blocked repeated contraction: %+v %+v", j.Operation, f.Status.Conditions)
	}
	if err := p.client.Delete(t.Context(), survivor); err != nil {
		t.Fatal(err)
	}
	reader.expired["pod-1"] = true
	// A fully settled historical member can also renew; its former completion
	// must not authorize disappearance after this contrary observation.
	reader.nodes = append(reader.nodes, "pod-2")
	reader.expired["pod-2"] = false
	step()
	if !j.BucketHistory[2].ExpiryInvalidated {
		t.Fatal("historical lease renewal did not persist invalidation")
	}
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-2" })
	step()
	if !j.Operation.SettledAt.IsZero() {
		t.Fatal("renewed historical member disappeared without new expiry proof")
	}
	reader.nodes = append(reader.nodes, "pod-2")
	reader.expired["pod-2"] = true
	step()
	step()
	reader.now = reader.now.Add(11 * time.Second)
	step()
	if j.Operation != nil || j.Applied != 2 || len(j.BucketHistory) != 4 || len(j.History) != 2 {
		t.Fatal("repeated shrink/grow lost authority")
	}
}

func TestBucketCancellationRetainsAdmittedGenerations(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	j.Operation.BucketCandidates = []bucketSession{{Node: "pod-uid", Generation: "generation", Container: "pod-uid/container/0", Retired: true}}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	f = desiredCount(t, r, f, 4)
	for range 8 {
		reconcile(t, r, f)
	}
	j = getJournal(t, r, f)
	if len(j.BucketHistory) != 1 || j.BucketHistory[0].Node != "pod-uid" || j.BucketHistory[0].Retired || j.Applied != 4 {
		t.Fatal("cancellation forgot Bucket admission")
	}
}

func TestBucketAutomaticNegativeHistorySurvivesBlockedReconcile(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = enableCapacity(t, r, f, "Automatic")
	now := time.Unix(10000, 0)
	c := &syntheticCollector{now: now, count: 3, cpu: 1000}
	r.Collector = c
	r.now = func() time.Time { return now }
	source := &inventoryReader{node: "unknown", gen: "generation", epoch: 1, now: now}
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	j := getJournal(t, r, f)
	state := capacity.Evaluate(*f.Spec.Capacity, capacity.State{LastManual: f.Spec.Replicas}, c.Collect(t.Context(), f), 3)
	state.LowSince = now.Add(-time.Hour)
	state.LowSamples = 100
	j.Capacity = &state
	j.Operation = &lifecycleOperation{ID: "automatic", Phase: "Blocked", From: 3, To: 2, Automatic: true, ManualBaseline: f.Spec.Replicas, PolicyHash: state.Config, StartedAt: now, Deadline: now.Add(operationBudget)}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, f)
	restored := getJournal(t, r, f)
	if !restored.Capacity.LowSince.IsZero() || restored.Capacity.LowSamples != 0 || restored.Applied != 3 || restored.Operation.Phase != "Blocked" {
		t.Fatal("blocked production automatic request revived old low-demand qualification")
	}
}

func TestBucketStatusSeparatesRetiredLivenessFromUnresolvedHistory(t *testing.T) {
	p, f, j, opts, _ := bucketPreflightSetup(t)
	j.Version = 5
	j.Initial = 3
	j.Applied = 3
	j.RuntimeImage = Image
	j.Operation = nil
	for _, s := range j.Inventory.Sessions {
		j.BucketHistory = append(j.BucketHistory, bucketSession{Node: s.Node, Generation: s.Generation, Container: s.Container})
	}
	j.BucketHistory[2].Retired = true
	j.Inventory.Blocker = "HistoricalSessionUnresolved"
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	if err := p.client.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Options: opts}
	for _, name := range []string{"admitted", "unknown generation", "transport unavailable"} {
		if name == "unknown generation" {
			j.Inventory.Sessions[0].Generation = "unknown"
		}
		if name == "transport unavailable" {
			j.Inventory.Sessions[0].Generation = j.BucketHistory[0].Generation
			j.Inventory.Blocker = "RecoveryInventoryUnavailable"
		}
		if err := r.saveJournal(t.Context(), res, j); err != nil {
			t.Fatal(err)
		}
		if _, err := r.report(t.Context(), f, "LifecycleProgress", "test observation", 0, false); err != nil {
			t.Fatal(err)
		}
		if f.Status.Lifecycle.RetiredBucketSessions != 1 {
			t.Fatal("physical liveness unknown count lost")
		}
		if (f.Status.Lifecycle.EvidenceBlocker == "") != (name == "admitted") {
			t.Fatalf("wrong evidence diagnosis for %s: %+v", name, f.Status.Lifecycle)
		}
	}
}

func TestBucketStrictPlacementChecksEveryVictim(t *testing.T) {
	f := fixture("az", "bucket-az", "Bucket")
	candidates := map[types.UID]bucketCandidate{
		"a1": {Hostname: "a1", Zone: "us-east-1a"}, "a2": {Hostname: "a2", Zone: "us-east-1a"},
		"b1": {Hostname: "b1", Zone: "us-east-1b"}, "b2": {Hostname: "b2", Zone: "us-east-1b"},
	}
	if err := validateBucketPlacement(f, candidates, true); err != nil {
		t.Fatal(err)
	}
	delete(candidates, "b2")
	if err := validateBucketPlacement(f, candidates, false); err != nil {
		t.Fatal("2/1 is currently valid strict spread")
	}
	if err := validateBucketPlacement(f, candidates, true); err == nil {
		t.Fatal("removing sole AZ member admitted")
	}
	f.Spec.Placement.Mode = "Relaxed"
	if err := validateBucketPlacement(f, candidates, true); err != nil {
		t.Fatal(err)
	}
	candidates["b1"] = bucketCandidate{Hostname: "b1", Zone: "us-east-1c"}
	if err := validateBucketPlacement(f, candidates, false); err == nil {
		t.Fatal("relaxation escaped zone allowlist")
	}
	f.Spec.Placement.Mode = "Strict"
	candidates["b1"] = bucketCandidate{Hostname: "a1", Zone: "us-east-1b"}
	if err := validateBucketPlacement(f, candidates, false); err == nil {
		t.Fatal("strict shared hostname accepted")
	}
}
