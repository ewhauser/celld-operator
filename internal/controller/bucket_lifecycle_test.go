package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBucketManualContractionCrashReplayAndRenewingOldLease(t *testing.T) {
	for _, contrary := range []string{"renewal", "replacement"} {
		t.Run(contrary, func(t *testing.T) { testBucketManualContractionCrashReplay(t, contrary, false) })
	}
}

func testBucketManualContractionCrashReplay(t *testing.T, contrary string, ordered bool) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Replicas = 2
	victimName := "pod-2"
	if ordered {
		f.Spec.BucketWorkload = "Ordered"
		f.Spec.Placement.AZCount = 2
		f.Spec.Placement.Zones = []string{"us-east-1a", "us-east-1b"}
		victimName = f.Name + "-2"
		if contrary == "wrong-victim" {
			victimName = f.Name + "-1"
		}
		for i := range 3 {
			pod := &corev1.Pod{}
			if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: fmt.Sprintf("pod-%d", i)}, pod); err != nil {
				t.Fatal(err)
			}
			if err := p.client.Delete(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			pod.Name = fmt.Sprintf("%s-%d", f.Name, i)
			pod.ResourceVersion = ""
			pod.OwnerReferences[0].Kind = "StatefulSet"
			pod.OwnerReferences[0].Name = f.Name
			pod.OwnerReferences[0].UID = j.WorkloadUID
			pod.Spec = podTemplate(f, opts).Spec
			pod.Spec.SchedulingGates = nil
			pod.Spec.NodeName = fmt.Sprintf("host-pod-%d", i)
			zone := f.Spec.Placement.Zones[i%2]
			pod.Spec.NodeSelector = map[string]string{corev1.LabelTopologyZone: zone}
			if err := p.client.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			node := &corev1.Node{}
			if err := p.client.Get(t.Context(), client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
				t.Fatal(err)
			}
			node.Labels[corev1.LabelTopologyZone] = zone
			if err := p.client.Update(t.Context(), node); err != nil {
				t.Fatal(err)
			}
		}
	}
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
	if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: victimName}, pod); err != nil {
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
	if ordered && contrary == "wrong-victim" {
		reader.expired = map[string]bool{"pod-1": true}
		reader.now = reader.now.Add(time.Minute)
		step()
		if j.Operation == nil || !j.Operation.SettledAt.IsZero() {
			t.Fatal("wrong ordinal removal completed")
		}
		return
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
	if ordered {
		return
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
	w.GetAnnotations()[operationKey] = "addition"
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
	if j.Operation != nil || j.Applied != 2 || len(j.BucketHistory) != 4 || len(j.History) != 1 {
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
		if _, err := r.report(t.Context(), f, nil, "LifecycleProgress", "test observation", false); err != nil {
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

func TestOrderedBucketContractionCrashReplay(t *testing.T) {
	for _, contrary := range []string{"renewal", "replacement", "wrong-victim"} {
		t.Run(contrary, func(t *testing.T) { testBucketManualContractionCrashReplay(t, contrary, true) })
	}
}

// Retirement is membership state, not expiry proof. A journal entry written
// before ExpiryObserved existed is Retired without it, and readJournal does not
// backfill it, so both flags must be consulted: only positive expiry authority
// may let the adapter tolerate a garbage-collected writer record.
func TestBucketRetiredHistoryWithoutPositiveExpiryIsNotResolved(t *testing.T) {
	for _, observed := range []bool{false, true} {
		name := "without-expiry-proof"
		if observed {
			name = "with-expiry-proof"
		}
		t.Run(name, func(t *testing.T) {
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
			retired := false
			for i := range j.BucketHistory {
				if j.BucketHistory[i].Node != "pod-1" {
					continue
				}
				if !j.BucketHistory[i].Retired || !j.BucketHistory[i].ExpiryObserved {
					t.Fatalf("writer was not retired with positive expiry: %+v", j.BucketHistory[i])
				}
				j.BucketHistory[i].ExpiryObserved = observed
				retired = true
			}
			if !retired {
				t.Fatal("no retired writer in history")
			}
			// The runtime garbage-collects the retired writer's record.
			reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-1" })
			_, err := r.admitBucketHistory(t.Context(), f, j)
			if observed {
				if err != nil {
					t.Fatalf("positive expiry proof did not resolve the missing record: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "unresolved bucket writer record missing") {
				t.Fatalf("retirement alone resolved a missing writer record: %v", err)
			}
		})
	}
}

// retiredBucketFleet admits three writers, then removes pod-1 and leaves an
// issued contraction in Recovering whose candidate sweep can no longer succeed:
// the surviving Pod count no longer matches the count the operation was issued
// against, which is what a replacement stuck Pending on a cordoned node does to
// a real pass.
func retiredBucketFleet(t *testing.T) (*Reconciler, *fleet.CelldFleet, *lifecycleJournal, *bucketReader) {
	t.Helper()
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
	for i := range j.BucketHistory {
		if j.BucketHistory[i].Node == "pod-1" {
			j.BucketHistory[i].Retired = true
			j.BucketHistory[i].ExpiryObserved = false
			j.BucketHistory[i].ExpiryInvalidated = false
		}
	}
	j.Operation = &lifecycleOperation{ID: "op", From: 3, To: 2, Phase: "Recovering", BucketCandidates: slices.Clone(j.BucketHistory)}
	if _, _, err := r.bucketAssessment(t.Context(), f, j, 2, true); err == nil {
		t.Fatal("expected this pass to fail before reading the retired writer's record")
	}
	return r, f, j, reader
}

// observed reports the expiry state recorded for one node across both record
// sets a post-effect pass carries.
func observedExpiry(j *lifecycleJournal, node string) (int, int) {
	proofs, records := 0, 0
	for _, sessions := range [][]bucketSession{j.Operation.BucketCandidates, j.BucketHistory} {
		for _, s := range sessions {
			if s.Node != node {
				continue
			}
			records++
			if resolvedBucketSession(s) {
				proofs++
			}
		}
	}
	return proofs, records
}

// The readable-expired window is about a second wide, and the proof is
// unrecoverable once the pinned runtime deletes the record. A pass must
// therefore take the reading before the candidate sweep, placement validation
// and survivor collection that can each abort it -- and must persist what it
// read even though the rest of that same pass fails.
func TestBucketRetirementExpiryRecordedBeforeAFailingSweep(t *testing.T) {
	r, f, j, reader := retiredBucketFleet(t)
	reader.expired = map[string]bool{"pod-1": true}
	probe := r.observeRetirementExpiry(t.Context(), f, j, [][]bucketSession{j.Operation.BucketCandidates, j.BucketHistory})
	if !probe.Changed || probe.Pending {
		t.Fatalf("expired record was not recorded on the pass that failed: %+v", probe)
	}
	proofs, records := observedExpiry(j, "pod-1")
	if records == 0 || proofs != records {
		t.Fatalf("expiry proof recorded in %d of %d records", proofs, records)
	}
	// The runtime now garbage-collects the record. The proof taken before the
	// sweep failed must carry the operation instead of wedging it forever.
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-1" })
	_, _, err := r.bucketAssessment(t.Context(), f, j, 2, true)
	if err != nil && strings.Contains(err.Error(), "unresolved bucket writer record missing") {
		t.Fatalf("recorded expiry did not survive the record's deletion: %v", err)
	}
}

// Absence is never expiry: a record already gone before anything read it must
// leave the operation blocked, however fast the controller polls.
func TestBucketRetirementProbeNeverInventsExpiry(t *testing.T) {
	r, f, j, reader := retiredBucketFleet(t)
	reader.expired = map[string]bool{"pod-1": true}
	reader.nodes = slices.DeleteFunc(reader.nodes, func(node string) bool { return node == "pod-1" })
	probe := r.observeRetirementExpiry(t.Context(), f, j, [][]bucketSession{j.Operation.BucketCandidates, j.BucketHistory})
	if probe.Changed {
		t.Fatal("a missing record was promoted into positive expiry proof")
	}
	if !probe.Pending {
		t.Fatal("a writer with no expiry proof was reported resolved")
	}
	if proofs, _ := observedExpiry(j, "pod-1"); proofs != 0 {
		t.Fatalf("%d expiry proofs recorded for a record nothing read", proofs)
	}
	_, _, err := r.bucketAssessment(t.Context(), f, j, 2, true)
	if err == nil || !strings.Contains(err.Error(), "unresolved bucket writer record missing") {
		t.Fatalf("missing record did not block the assessment: %v", err)
	}
}

// Cadence follows the window, not the phase: poll every second while a retired
// writer's record may still be readable, and hand the ordinary reconcile delay
// back as soon as the reading is taken.
func TestBucketRetirementWindowCadence(t *testing.T) {
	r, f, j, reader := retiredBucketFleet(t)
	ordinary := ctrl.Result{RequeueAfter: reconcileDelay(f)}
	if ordinary.RequeueAfter <= retirementWindow {
		t.Fatalf("ordinary cadence %s does not exceed the retirement window", ordinary.RequeueAfter)
	}
	// The writer is gone but its lease has not elapsed yet: nothing to record,
	// and the window is still ahead, so keep polling inside it.
	records := [][]bucketSession{j.Operation.BucketCandidates, j.BucketHistory}
	probe := r.observeRetirementExpiry(t.Context(), f, j, records)
	if probe.Changed {
		t.Fatal("a live lease was recorded as expiry")
	}
	if !probe.Pending {
		t.Fatal("an unresolved retirement did not hold the tight cadence")
	}
	if got := tighten(ordinary, probe.Pending); got.RequeueAfter != retirementWindow {
		t.Fatalf("requeue %s, want %s while the window may be open", got.RequeueAfter, retirementWindow)
	}
	// The lease elapses and the record is read. The proof is durable now, so the
	// fleet goes back to its ordinary cadence.
	reader.expired = map[string]bool{"pod-1": true}
	probe = r.observeRetirementExpiry(t.Context(), f, j, records)
	if !probe.Changed || probe.Pending {
		t.Fatalf("expired record was not recorded: %+v", probe)
	}
	if got := tighten(ordinary, probe.Pending); got.RequeueAfter != ordinary.RequeueAfter {
		t.Fatalf("requeue %s, want the ordinary %s once expiry is observed", got.RequeueAfter, ordinary.RequeueAfter)
	}
	// A second pass has nothing left to read and must not re-poll tightly.
	probe = r.observeRetirementExpiry(t.Context(), f, j, records)
	if probe.Changed || probe.Pending {
		t.Fatalf("resolved retirement kept the tight cadence: %+v", probe)
	}
}
