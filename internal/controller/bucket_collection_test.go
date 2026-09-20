package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// countingCollector counts the Metrics Server plus /state fan-outs a pass
// performs and, through advance, injects the wall-clock latency a real
// Collector spends inside one call.
type countingCollector struct {
	t       *testing.T
	client  client.Client
	reader  *bucketReader
	advance time.Duration
	calls   int
}

func (c *countingCollector) Collect(ctx context.Context, _ *fleet.CelldFleet) capacity.Observation {
	c.t.Helper()
	c.calls++
	c.reader.now = c.reader.now.Add(c.advance)
	pods := &corev1.PodList{}
	if err := c.client.List(ctx, pods); err != nil {
		c.t.Fatal(err)
	}
	o := capacity.Observation{At: c.reader.now, Complete: true}
	for i := range pods.Items {
		id, _ := podIdentity(&pods.Items[i])
		o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: c.reader.now, RuntimeReceived: c.reader.now, MetricsAt: c.reader.now, MetricsReceived: c.reader.now, Window: 15 * time.Second})
	}
	return o
}

// sweepCounter counts Node reads. Every bucket candidate sweep reads each
// distinct host exactly once, so a sweep of the three-pod fixture is three Node
// reads, and nothing else on these paths reads a Node.
const fixturePods = 3

type sweepCounter struct{ nodeGets int }

func (s *sweepCounter) sweeps(t *testing.T) int {
	t.Helper()
	if s.nodeGets%fixturePods != 0 {
		t.Fatalf("%d Node reads is not a whole number of %d-pod candidate sweeps", s.nodeGets, fixturePods)
	}
	return s.nodeGets / fixturePods
}

func countingClient(inner client.Client, counter *sweepCounter) client.Client {
	return interceptor.NewClient(inner.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, o ...client.GetOption) error {
		if _, ok := obj.(*corev1.Node); ok {
			counter.nodeGets++
		}
		return c.Get(ctx, key, obj, o...)
	}})
}

// localBucketTemplate rewrites the fixture's workload template and pods so the
// candidate sweep still admits them once the pass runs with LocalTest options,
// which Automatic contraction requires outside the AWS release gate.
func localBucketTemplate(t *testing.T, p *ProductionEvidence, f *fleet.CelldFleet, opts Options) {
	t.Helper()
	rs := &appsv1.ReplicaSet{}
	if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "bucket-rs"}, rs); err != nil {
		t.Fatal(err)
	}
	rs.Spec.Template = podTemplate(f, opts)
	if err := p.client.Update(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
	pods := &corev1.PodList{}
	if err := p.client.List(t.Context(), pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		node := pod.Spec.NodeName
		pod.Spec = *rs.Spec.Template.Spec.DeepCopy()
		pod.Spec.NodeName = node
		if err := p.client.Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
	}
}

// automaticContraction prepares an issued-in-this-pass Automatic Bucket
// removal: the policy qualification the pass rechecks, and the durable intent
// it re-admits before the replica CAS.
func automaticContraction(t *testing.T, p *ProductionEvidence, f *fleet.CelldFleet, j *lifecycleJournal, opts Options, reader *bucketReader) (*fleet.CelldStorageReservation, client.Object) {
	t.Helper()
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Replicas = 3
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Automatic", MinReplicas: 1, MaxReplicas: 5}
	f.Default()
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	localBucketTemplate(t, p, f, opts)
	observation := capacity.Observation{At: reader.now, Complete: true}
	state := capacity.Evaluate(*f.Spec.Capacity, capacity.State{LastManual: f.Spec.Replicas}, observation, 3)
	state.LowSince = reader.now.Add(-time.Hour)
	state.LowSamples = 1000
	j.Version = 5
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Capacity = &state
	j.Operation = &lifecycleOperation{ID: "automatic", Phase: "Intent", From: 3, To: 2, Automatic: true, ManualBaseline: f.Spec.Replicas, PolicyHash: state.Config, StartedAt: reader.now, Deadline: reader.now.Add(operationBudget)}
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
	return res, w
}

// One reconcile pass asks the runtime and the cluster the same questions once.
// Before this, an Automatic contraction pass ran two full Collects (one inside
// the assessment, one again for the low-demand recheck) and a restart pass ran
// three candidate sweeps (the assessment's fenced pair, then a third for the
// placement check). The evidence was identical every time; only the latency the
// freshness window had to absorb doubled.
func TestBucketPassCollectsEvidenceOnce(t *testing.T) {
	t.Run("contraction", func(t *testing.T) {
		opts := Options{OperatorNamespace: "celld-system", LocalTest: true}
		p, f, j, _, reader := bucketPreflightSetup(t)
		p.now = func() time.Time { return reader.now }
		res, w := automaticContraction(t, p, f, j, opts, reader)
		counter := &sweepCounter{}
		p.client = countingClient(p.client, counter)
		collector := &countingCollector{t: t, client: p.client, reader: reader}
		r := &Reconciler{Client: p.client, Evidence: p, Options: opts, Collector: collector, now: p.now}
		for pass := range 4 {
			counter.nodeGets, collector.calls = 0, 0
			if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.contractBucket(t.Context(), f, res, j, w); err != nil {
				t.Fatal(err)
			}
			if collector.calls != 1 || counter.sweeps(t) != 2 {
				t.Fatalf("pass %d collected %d times over %d candidate sweeps, want 1 and 2", pass, collector.calls, counter.sweeps(t))
			}
			if replicas(w) == 2 {
				return
			}
		}
		t.Fatalf("automatic contraction never issued: %+v %+v", j.Operation, f.Status.Conditions)
	})
	t.Run("restart", func(t *testing.T) {
		p, f, j, opts, reader := bucketPreflightSetup(t)
		p.now = func() time.Time { return reader.now }
		p.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
			return maintenanceReader{Reader: reader}, nil
		}
		f.Spec.Replicas = 3
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "one"}
		if err := p.client.Update(t.Context(), f); err != nil {
			t.Fatal(err)
		}
		j.Version = 6
		j.RuntimeImage = Image
		j.Initial = 3
		j.Applied = 3
		j.Operation = nil
		j.Maintenance = &maintenanceOperation{ID: "restart", Kind: "Restart", Token: "one", Phase: "Capture", Deadline: reader.now.Add(time.Hour)}
		w := workload(f, opts).(*appsv1.Deployment)
		w.UID = j.WorkloadUID
		res := &fleet.CelldStorageReservation{Name: reservationName(f)}
		if err := p.client.Create(t.Context(), w); err != nil {
			t.Fatal(err)
		}
		if err := p.client.Create(t.Context(), res); err != nil {
			t.Fatal(err)
		}
		pods := &corev1.PodList{}
		if err := p.client.List(t.Context(), pods); err != nil {
			t.Fatal(err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			pod.Status.PodIP = "10.0.0." + strings.TrimPrefix(pod.Name, "pod-")
			if err := p.client.Status().Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
		}
		counter := &sweepCounter{}
		p.client = countingClient(p.client, counter)
		collector := &countingCollector{t: t, client: p.client, reader: reader}
		r := &Reconciler{Client: p.client, Evidence: p, Options: opts, Collector: collector, now: p.now}
		for _, phase := range []string{"Capture", "Authorized"} {
			if j.Maintenance.Phase != phase {
				t.Fatalf("expected phase %s, got %+v", phase, j.Maintenance)
			}
			counter.nodeGets, collector.calls = 0, 0
			if _, _, err := r.executeMaintenance(t.Context(), f, res, j, w); err != nil {
				t.Fatal(err)
			}
			if collector.calls != 1 || counter.sweeps(t) != 2 {
				t.Fatalf("%s collected %d times over %d candidate sweeps, want 1 and 2", phase, collector.calls, counter.sweeps(t))
			}
		}
		if j.Maintenance.Phase != "Recovering" {
			t.Fatalf("restart did not reach recovery: %+v %+v", j.Maintenance, f.Status.Conditions)
		}
	})
}

// A missed window must name the latency that caused it. The old message said
// only "bucket assessment expired or membership changed", which reads as a
// safety failure rather than a slow Metrics Server.
func TestBucketAssessmentReportsSlowCollection(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	collector := &countingCollector{t: t, client: p.client, reader: reader, advance: 7 * time.Second}
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, Collector: collector, now: p.now}
	_, _, err := r.bucketAssessment(t.Context(), f, j, 3, false)
	if err == nil {
		t.Fatal("a seven second collection satisfied a five second window")
	}
	for _, want := range []string{"evidence collection took 7s", "freshness window 5s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("blocker does not report %q: %v", want, err)
		}
	}
}

// Routine Metrics Server and /state latency is not staleness. A single three
// second collection leaves the complete evidence usable, and the pass issues.
// Measured the old way the same pass failed: the window ran from before the
// assessment's own Collect, and the Automatic low-demand recheck then spent a
// second three seconds inside it, so six seconds had elapsed by the preflight.
func TestBucketAssessmentToleratesCollectionLatency(t *testing.T) {
	opts := Options{OperatorNamespace: "celld-system", LocalTest: true}
	p, f, j, _, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	res, w := automaticContraction(t, p, f, j, opts, reader)
	collector := &countingCollector{t: t, client: p.client, reader: reader, advance: 3 * time.Second}
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, Collector: collector, now: p.now}
	for range 4 {
		if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.contractBucket(t.Context(), f, res, j, w); err != nil {
			t.Fatal(err)
		}
		if replicas(w) == 2 {
			if collector.calls > 4 {
				t.Fatalf("issued after %d collections", collector.calls)
			}
			return
		}
	}
	t.Fatalf("three second collection blocked contraction: %+v %+v", j.Operation, f.Status.Conditions)
}
