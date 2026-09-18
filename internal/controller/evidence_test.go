package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type inventoryReader struct {
	node, gen                     string
	epoch                         uint64
	now                           time.Time
	missing, partial, loss, stale bool
}

func (r *inventoryReader) Get(context.Context, string) ([]byte, error) {
	sampled := r.now
	if r.stale {
		sampled = sampled.Add(-time.Hour)
	}
	return json.Marshal(map[string]any{"node": r.node, "ownership_index_generation": r.gen, "peer_protocol": 5, "expires_ms": r.now.Add(time.Minute).UnixMilli(), "addr": "127.0.0.1:8081", "load": map[string]any{"sampled_ms": sampled.UnixMilli()}, "log": map[string]any{"active": true, "state": "open", "epoch": r.epoch, "ensemble": []string{}, "tiered": 0}})
}
func (r *inventoryReader) List(_ context.Context, prefix, token string) (v050.Page, error) {
	if token != "" {
		return v050.Page{}, errors.New("page unavailable")
	}
	p := v050.Page{Complete: true}
	if prefix == "nodes/" && !r.missing {
		p.Keys = []string{"nodes/" + r.node + ".json"}
	}
	if prefix == "log/" && r.partial {
		p.Complete = false
		p.Next = "next"
	}
	if prefix == "log/" && r.loss {
		p.Keys = []string{"log/old/gen.e1.loss.json"}
	}
	return p, nil
}
func TestProductionInventoryRetainsAmbiguousHistory(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			now := time.Now()
			f := fixture("alpha", "bucket-alpha", profile)
			pod := &corev1.Pod{Name: "alpha-0", Namespace: f.Namespace, UID: "pod-uid", Labels: labels(f), Spec: corev1.PodSpec{NodeName: "host", Containers: []corev1.Container{{Name: "celld", Image: Image}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}}}
			r := setup(t, f, pod)
			source := &inventoryReader{node: runtimeNode(f, pod), gen: "generation-1", epoch: 1, now: now}
			p := &ProductionEvidence{client: r.Client, now: func() time.Time { return now }, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
			inv, loss := p.Observe(t.Context(), f, recoveryInventory{})
			if loss != "" || len(inv.Sessions) != 1 || inv.Sessions[0].PodUID != "pod-uid" || inv.Sessions[0].Container != "pod-uid/container-1/0" || inv.Blocker != "SessionBindingUnqualified" {
				t.Fatalf("%+v", inv)
			}
			// Restart persistence is JSON, not in-memory provider state.
			data, _ := json.Marshal(inv)
			var restored recoveryInventory
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			source.gen = "generation-2"
			source.epoch = 2
			inv, _ = p.Observe(t.Context(), f, restored)
			if len(inv.Sessions) != 2 || inv.Sessions[0].Current || inv.Blocker != "HistoricalSessionUnresolved" {
				t.Fatalf("replacement erased history: %+v", inv)
			}
			source.missing = true
			inv, _ = p.Observe(t.Context(), f, inv)
			if len(inv.Sessions) != 2 || inv.Sessions[1].Current {
				t.Fatal("missing node erased history")
			}
			source.missing = false
			source.partial = true
			inv, _ = p.Observe(t.Context(), f, inv)
			if !inv.CheckedAt.IsZero() || inv.Blocker != "RecoveryInventoryUnavailable" {
				t.Fatal("partial inventory accepted")
			}
			source.partial = false
			source.stale = true
			source.gen = "generation-3"
			inv, _ = p.Observe(t.Context(), f, inv)
			if inv.Sessions[len(inv.Sessions)-1].Container != "" {
				t.Fatal("stale lease associated")
			}
			if err := r.Delete(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if stopped, err := p.Stopped(t.Context(), f, &lifecycleOperation{}); stopped || err == nil {
				t.Fatal("absence fenced process")
			}
		})
	}
}
func TestSurvivorCapacityProjectionAndUncertainty(t *testing.T) {
	p := fleet.CapacityPolicy{}
	p.Default()
	p.CPULowMillicores = 150
	now := time.Now()
	c := &syntheticCollector{now: now, count: 3, cpu: 120}
	o := c.Collect(t.Context(), nil)
	if err := ValidateSurvivors(p, o, []string{"0", "1", "2"}, "2", now); err == nil {
		t.Fatal("concentrated donor exceeds survivor capacity")
	}
	c.cpu = 10
	o = c.Collect(t.Context(), nil)
	if err := ValidateSurvivors(p, o, []string{"0", "1", "2"}, "2", now); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"missing", "replacement", "stale", "pressure", "backlog", "unready", "memory", "resident memory"} {
		t.Run(fault, func(t *testing.T) {
			o := c.Collect(t.Context(), nil)
			switch fault {
			case "missing":
				o.Complete = false
			case "replacement":
				o.Samples[0].Identity = "new"
			case "stale":
				o.Samples[0].RuntimeAt = now.Add(-time.Hour)
			case "pressure":
				o.Samples[0].Pressured = true
			case "backlog":
				o.Samples[0].Backlog = true
			case "unready":
				o.Samples[0].Ready = false
			case "memory":
				o.Samples[0].MemoryMiB = 999
			case "resident memory":
				o.Samples[0].RuntimeMemoryMiB = 999
			}
			if err := ValidateSurvivors(p, o, []string{"0", "1", "2"}, "2", now); err == nil {
				t.Fatal("fault passed")
			}
		})
	}
}
func TestProductionEvidenceIsPersistedByRunnablePath(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	now := time.Now()
	source := &inventoryReader{node: "unknown-writer", gen: "old", epoch: 1, now: now}
	r.Evidence = &ProductionEvidence{client: r.Client, now: func() time.Time { return now }, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	reconcile(t, r, f)
	if len(getJournal(t, r, f).Inventory.Sessions) != 1 {
		t.Fatal("inventory not journaled")
	}
	source.loss = true
	reconcile(t, r, f)
	if getJournal(t, r, f).Loss == "" {
		t.Fatal("loss not sticky")
	}
	// Even repeated loss observations may not starve an explicit safe addition.
	f = desiredCount(t, r, f, 4)
	for range 5 {
		reconcile(t, r, f)
	}
	if getJournal(t, r, f).Applied != 4 {
		t.Fatal("loss blocked safe addition")
	}
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Lifecycle.SessionCount != 1 {
		t.Fatal("missing evidence projection")
	}
}

func TestPartialLossAndKnownIdentityReactivation(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	now := time.Now()
	source := &inventoryReader{node: "alpha-3", gen: "old-generation", epoch: 1, now: now, partial: true, loss: true}
	r.Evidence = &ProductionEvidence{client: r.Client, now: func() time.Time { return now }, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	if j.Loss == "" || len(j.Inventory.Sessions) != 1 {
		t.Fatal("partial scan discarded negative evidence")
	}
	f = desiredCount(t, r, f, 4)
	for range 4 {
		reconcile(t, r, f)
	}
	if getJournal(t, r, f).Applied != 3 {
		t.Fatal("reactivated unbound historical identity")
	}
}

func TestInventoryContainerReplacementAndEpochHistory(t *testing.T) {
	now := time.Now()
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	pod := &corev1.Pod{Name: "alpha-0", Namespace: f.Namespace, UID: "uid", Labels: labels(f), Spec: corev1.PodSpec{NodeName: "host", Containers: []corev1.Container{{Name: "celld", Image: Image}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "first", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}}}
	r := setup(t, f, pod)
	source := &inventoryReader{node: pod.Name, gen: "generation", epoch: 1, now: now}
	p := &ProductionEvidence{client: r.Client, now: func() time.Time { return now }, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	inv, _ := p.Observe(t.Context(), f, recoveryInventory{})
	source.epoch = 2
	inv, _ = p.Observe(t.Context(), f, inv)
	if len(inv.Sessions) != 2 || !inv.Sessions[0].Current || !inv.Sessions[1].Current {
		t.Fatal("same-process epoch history lost")
	}
	current := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pod), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ContainerStatuses[0].ContainerID = "second"
	current.Status.ContainerStatuses[0].RestartCount = 1
	if err := r.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	inv, _ = p.Observe(t.Context(), f, inv)
	if len(inv.Sessions) != 3 || inv.Sessions[0].Current || inv.Sessions[1].Current || inv.Blocker != "HistoricalSessionUnresolved" {
		t.Fatalf("replacement erased previous process: %+v", inv)
	}
}
