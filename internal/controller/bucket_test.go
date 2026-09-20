package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type bucketReader struct {
	now        time.Time
	nodes      []string
	hook       func()
	expired    map[string]bool
	generation map[string]string
}

func (r *bucketReader) List(_ context.Context, prefix, _ string) (v050.Page, error) {
	if r.hook != nil {
		hook := r.hook
		r.hook = nil
		hook()
	}
	page := v050.Page{Complete: true}
	if prefix == "nodes/" {
		for _, n := range r.nodes {
			page.Keys = append(page.Keys, "nodes/"+n+".json")
		}
	}
	return page, nil
}
func (r *bucketReader) Get(_ context.Context, key string) ([]byte, error) {
	node := strings.TrimSuffix(strings.TrimPrefix(key, "nodes/"), ".json")
	expires := r.now.Add(time.Minute).UnixMilli()
	if r.expired[node] {
		expires = r.now.Add(-time.Second).UnixMilli()
	}
	generation := r.generation[node]
	if generation == "" {
		generation = "generation"
	}
	return json.Marshal(map[string]any{"node": node, "ownership_index_generation": generation, "peer_protocol": 5, "expires_ms": expires, "load": map[string]any{"sampled_ms": r.now.UnixMilli()}})
}

func bucketPreflightSetup(t *testing.T) (*ProductionEvidence, *fleet.CelldFleet, *lifecycleJournal, Options, *bucketReader) {
	t.Helper()
	f := fixture("bucket", "bucket-data", "Bucket")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	now := time.Unix(10000, 0)
	opts := Options{OperatorNamespace: "celld-system"}
	rs := &appsv1.ReplicaSet{Name: "bucket-rs", Namespace: f.Namespace, UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: f.Name, UID: "deployment-uid", Controller: new(true)}}, Spec: appsv1.ReplicaSetSpec{Template: podTemplate(f, opts)}}
	objects := []client.Object{f, rs}
	j := &lifecycleJournal{WorkloadUID: "deployment-uid", Operation: &lifecycleOperation{ID: "op", From: 3, To: 2, Phase: "Blocked"}, Inventory: recoveryInventory{CheckedAt: now, Blocker: "SessionBindingUnqualified"}}
	reader := &bucketReader{now: now}
	for i := range 3 {
		name := fmt.Sprintf("pod-%d", i)
		pod := &corev1.Pod{Name: name, Namespace: f.Namespace, UID: types.UID(name), Labels: labels(f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: new(true)}}, Spec: *rs.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}}}}
		pod.Spec.NodeName = "host-" + name
		objects = append(objects, pod, &corev1.Node{Name: pod.Spec.NodeName, UID: types.UID("node-" + name), Labels: map[string]string{corev1.LabelHostname: pod.Spec.NodeName, corev1.LabelTopologyZone: "us-east-1a"}})
		id, _ := podIdentity(pod)
		j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: name, Pod: name, PodUID: name, Container: id, Generation: "generation", Current: true, Association: "ObservedUnverified"})
		reader.nodes = append(reader.nodes, name)
	}
	r := setup(t, objects...)
	p := &ProductionEvidence{client: r.Client, now: func() time.Time { return now }, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return reader, nil }}
	return p, f, j, opts, reader
}

func TestBucketEveryDeploymentCandidate(t *testing.T) {
	for _, name := range []string{"complete", "foreign owner", "owner replacement", "wrong posture", "injected posture", "missing configuration", "extra runtime configuration", "binary mount", "service account", "duplicate environment", "unknown candidate", "replacement", "history", "stale", "membership race"} {
		t.Run(name, func(t *testing.T) {
			p, f, j, opts, reader := bucketPreflightSetup(t)
			pod := &corev1.Pod{}
			if err := p.client.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "pod-2"}, pod); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "foreign owner":
				pod.OwnerReferences[0].Name = "unknown"
			case "owner replacement":
				pod.OwnerReferences[0].UID = "replacement"
			case "wrong posture":
				for i := range pod.Spec.Containers[0].Env {
					if pod.Spec.Containers[0].Env[i].Name == "CELLD_DURABILITY" {
						pod.Spec.Containers[0].Env[i].Value = "fleet"
					}
				}
			case "injected posture":
				pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{Name: "override"}}}
			case "missing configuration":
				pod.Spec.Containers[0].Env = pod.Spec.Containers[0].Env[:1]
			case "extra runtime configuration":
				pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "CELLD_UNKNOWN", Value: "1"})
			case "binary mount":
				pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "work", MountPath: "/usr/local/bin"})
			case "service account":
				pod.Spec.ServiceAccountName = "other"
			case "duplicate environment":
				pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, pod.Spec.Containers[0].Env[0])
			case "unknown candidate":
				j.Operation.From = 2
			case "replacement":
				j.Inventory.Sessions[2].Container = "old-container"
			case "history":
				j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: "historical", Generation: "old"})
			case "stale":
				j.Inventory.CheckedAt = j.Inventory.CheckedAt.Add(-time.Minute)
			case "membership race":
				reader.hook = func() {
					pod.OwnerReferences[0].UID = "replacement"
					if err := p.client.Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := p.client.Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			err := p.inspectBucket(t.Context(), f, j, opts)
			if name == "complete" {
				if err != nil {
					t.Fatal(err)
				}
				if stopped, err := p.Stopped(t.Context(), f, j.Operation); stopped || err == nil {
					t.Fatal("preflight became process fence")
				}
			} else if err == nil {
				t.Fatal("unsafe candidate admitted")
			}
		})
	}
}

type bucketCapacityCollector struct {
	observation capacity.Observation
}

func (c bucketCapacityCollector) Collect(context.Context, *fleet.CelldFleet) capacity.Observation {
	return c.observation
}

func TestBucketPreflightNeverAuthorizesContraction(t *testing.T) {
	p, f, j, opts, _ := bucketPreflightSetup(t)
	observation := capacity.Observation{At: p.now(), Complete: true}
	for _, s := range j.Inventory.Sessions {
		observation.Samples = append(observation.Samples, capacity.Sample{Identity: s.Container, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: p.now(), RuntimeReceived: p.now(), MetricsAt: p.now(), MetricsReceived: p.now(), Window: 15 * time.Second})
	}
	r := &Reconciler{Client: p.client, Evidence: p, Collector: bucketCapacityCollector{observation}, Options: opts, now: p.now}
	reason, _ := r.productionRemovalBlock(t.Context(), f, j)
	if reason != "SessionBindingUnqualified" {
		t.Fatalf("binding unexpectedly accepted: %s", reason)
	}
	j.Inventory.Blocker = "" // Even synthetic binding cannot bypass completion authority.
	reason, _ = r.productionRemovalBlock(t.Context(), f, j)
	if reason != "BucketCompletionUnqualified" {
		t.Fatalf("completion unexpectedly accepted: %s", reason)
	}
	for i := range observation.Samples {
		observation.Samples[i].CPU = 1000
		r.Collector = bucketCapacityCollector{observation}
		reason, _ = r.productionRemovalBlock(t.Context(), f, j)
		if reason != "CapacityUncertain" {
			t.Fatalf("candidate %d omitted: %s", i, reason)
		}
		observation.Samples[i].CPU = 10
	}
}

// The reconciler's client is uncached, so a per-pod owner fetch is a real API
// call per pod. Every pod of a fleet shares one owner: one Get must cover them all.
func TestBucketCandidatesFetchesOwningStatefulSetOncePerCall(t *testing.T) {
	f := fixture("ordered", "ordered-data", "Bucket")
	f.Spec.BucketWorkload = "Ordered"
	opts := Options{OperatorNamespace: "celld-system"}
	now := time.Unix(10000, 0)
	sts := workload(f, opts).(*appsv1.StatefulSet)
	sts.UID = "workload"
	objects := []client.Object{f, sts}
	for i := range 5 {
		name := fmt.Sprintf("%s-%d", f.Name, i)
		zone := f.Spec.Placement.Zones[i%len(f.Spec.Placement.Zones)]
		pod := &corev1.Pod{
			Name: name, Namespace: f.Namespace, UID: types.UID(name), Labels: labels(f),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: sts.UID, Controller: new(true)}},
			Spec:            *sts.Spec.Template.Spec.DeepCopy(),
			Status: corev1.PodStatus{
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-" + name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}}},
			},
		}
		pod.Spec.SchedulingGates = nil
		pod.Spec.NodeName = "host-" + name
		pod.Spec.NodeSelector = map[string]string{corev1.LabelTopologyZone: zone}
		objects = append(objects, pod, &corev1.Node{Name: pod.Spec.NodeName, UID: types.UID("node-" + name), Labels: map[string]string{corev1.LabelHostname: pod.Spec.NodeName, corev1.LabelTopologyZone: zone}})
	}
	r := setup(t, objects...)
	gets := 0
	counting := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, o ...client.GetOption) error {
		if _, ok := obj.(*appsv1.StatefulSet); ok {
			gets++
		}
		return c.Get(ctx, key, obj, o...)
	}})
	p := &ProductionEvidence{client: counting, now: func() time.Time { return now }}
	j := &lifecycleJournal{WorkloadUID: sts.UID, Operation: &lifecycleOperation{ID: "op", From: 5, To: 4, Phase: "Blocked"}}
	candidates, err := p.bucketCandidates(t.Context(), f, j, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 5 {
		t.Fatalf("admitted %d of 5 bucket candidates", len(candidates))
	}
	if gets != 1 {
		t.Fatalf("fetched the shared StatefulSet %d times for one candidate sweep, want 1", gets)
	}
}
