package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// csiDeletionFinalizer is the external-provisioner finalizer a conforming CSI
// driver keeps on a Delete PV until its backing storage is gone.
const csiDeletionFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

// revisionLabel is the label the StatefulSet controller puts on each Pod,
// naming the template revision it was created from.
const revisionLabel = "controller-revision-hash"

// operationFixture simulates the Kubernetes workload controller, scheduler and
// CSI binding around one fleet.
type operationFixture struct {
	t     *testing.T
	r     *Reconciler
	f     *fleet.CelldFleet
	clock time.Time
	cpu   int64
	// admit simulates mutating admission on each Pod the workload controller creates.
	admit func(*corev1.PodSpec)
	// hold stops the simulated StatefulSet rollout, as an unready Pod would.
	hold bool
}
type fixtureCollector struct{ x *operationFixture }

func (c fixtureCollector) Collect(ctx context.Context, _ *fleet.CelldFleet) capacity.Observation {
	x := c.x
	pods := &corev1.PodList{}
	if err := x.r.List(ctx, pods, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := capacity.Observation{At: x.clock, Complete: true}
	for _, p := range pods.Items {
		id, _ := podIdentity(&p)
		out.Samples = append(out.Samples, capacity.Sample{Identity: id, Ready: true, CPU: x.cpu, MemoryMiB: 10, RuntimeMemoryMiB: 10, RuntimeAt: x.clock, RuntimeReceived: x.clock, MetricsAt: x.clock, MetricsReceived: x.clock, Window: 15 * time.Second})
	}
	return out
}

func newOperationFixture(t *testing.T, profile string) *operationFixture {
	t.Helper()
	deployment := profile == "Deployment"
	if deployment {
		profile = "Bucket"
	}
	f := fixture("alpha", "bucket-alpha", profile)
	if profile == "Bucket" && !deployment {
		f.Spec.BucketWorkload = "Ordered"
	}
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	x := &operationFixture{t: t, f: f, clock: time.Now().UTC().Truncate(time.Millisecond), cpu: 10}
	x.r = setup(t, f)
	x.r.now = func() time.Time { return x.clock }
	x.r.Collector = fixtureCollector{x}
	reconcile(t, x.r, f)
	reconcile(t, x.r, f)
	x.syncWorkload()
	return x
}
func (x *operationFixture) state() *fleetState { return getCurrentState(x.t, x.r, x.f) }
func (x *operationFixture) workload() client.Object {
	w := emptyObject(workload(x.f, x.r.Options))
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), w); err != nil {
		x.t.Fatal(err)
	}
	return w
}
func (x *operationFixture) step() *fleet.CelldFleet { x.t.Helper(); return reconcile(x.t, x.r, x.f) }
func (x *operationFixture) desired(n int32)         { x.f = desiredCount(x.t, x.r, x.f, n) }

// converge runs the controller and simulated Kubernetes until the fleet
// reports Provisioned.
func (x *operationFixture) converge() {
	x.t.Helper()
	var f *fleet.CelldFleet
	for range 40 {
		f = x.step()
		x.syncWorkload()
		if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c != nil && c.Reason == "Provisioned" {
			return
		}
	}
	x.t.Fatalf("fleet never converged: %+v", meta.FindStatusCondition(f.Status.Conditions, "Ready"))
}
func (x *operationFixture) edit(edit func(*fleet.CelldFleet)) {
	x.t.Helper()
	f := &fleet.CelldFleet{}
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), f); err != nil {
		x.t.Fatal(err)
	}
	edit(f)
	if err := x.r.Update(x.t.Context(), f); err != nil {
		x.t.Fatal(err)
	}
	x.f = f
}

// templateRevision stands in for the StatefulSet controller's revision hash.
func templateRevision(t *testing.T, template corev1.PodTemplateSpec) string {
	t.Helper()
	b, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	return digest(b)[:10]
}

func (x *operationFixture) pod(name string) *corev1.Pod {
	x.t.Helper()
	p := &corev1.Pod{}
	if err := x.r.Get(x.t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: name}, p); err != nil {
		x.t.Fatal(err)
	}
	return p
}

// Simulate only the Kubernetes workload controller, scheduler and CSI binding.
// A StatefulSet creates each missing Pod at the update revision. Under
// RollingUpdate it replaces one outdated Pod per sync, highest ordinal first;
// under OnDelete it never replaces a Pod that nothing deleted.
func (x *operationFixture) syncWorkload() {
	t := x.t
	ctx := t.Context()
	x.syncStorage()
	w := x.workload()
	n := replicas(w)
	revision := ""
	rolling := false
	if sts, ok := w.(*appsv1.StatefulSet); ok {
		revision = templateRevision(t, sts.Spec.Template)
		rolling = sts.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && !x.hold
	}
	pods := &corev1.PodList{}
	if err := x.r.List(ctx, pods, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		t.Fatal(err)
	}
	outdated := -1
	for _, p := range pods.Items {
		var ordinal int
		_, _ = fmt.Sscanf(p.Name, x.f.Name+"-%d", &ordinal)
		if ordinal >= int(n) {
			if err := x.r.Delete(ctx, &p, client.Preconditions{UID: &p.UID}); err != nil {
				t.Fatal(err)
			}
		} else if rolling && p.Labels[revisionLabel] != revision {
			outdated = max(outdated, ordinal)
		}
	}
	if outdated >= 0 {
		p := x.pod(fmt.Sprintf("%s-%d", x.f.Name, outdated))
		if err := x.r.Delete(ctx, p, client.Preconditions{UID: &p.UID}); err != nil {
			t.Fatal(err)
		}
	}
	updated := int32(0)
	for i := range n {
		name := fmt.Sprintf("%s-%d", x.f.Name, i)
		p := &corev1.Pod{}
		err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: name}, p)
		if err == nil {
			if revision == "" || p.Labels[revisionLabel] == revision {
				updated++
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		updated++
		host := fmt.Sprintf("host-%d", i)
		node := &corev1.Node{Name: host, UID: types.UID(host), Labels: map[string]string{corev1.LabelTopologyZone: "us-east-1a", corev1.LabelHostname: host}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if err := x.r.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		spec := podTemplate(x.f, x.r.Options).Spec
		spec.SchedulingGates = nil
		spec.NodeName = host
		owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: w.GetUID(), Controller: new(true)}
		podLabels := labels(x.f)
		switch w := w.(type) {
		case *appsv1.StatefulSet:
			spec.Containers[0].Image = w.Spec.Template.Spec.Containers[0].Image
			podLabels[revisionLabel] = revision
		case *appsv1.Deployment:
			spec.Containers[0].Image = w.Spec.Template.Spec.Containers[0].Image
			rs := &appsv1.ReplicaSet{Name: x.f.Name + "-rs", Namespace: x.f.Namespace, UID: types.UID(x.f.Name + "-rs"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: x.f.Name, UID: w.UID, Controller: new(true)}}}
			if err := x.r.Create(ctx, rs); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
			owner.Kind = "ReplicaSet"
			owner.Name = rs.Name
			owner.UID = rs.UID
		}
		if x.f.Spec.Profile == "PersistentFleet" {
			claim := &corev1.PersistentVolumeClaim{}
			err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "data-" + name}, claim)
			if apierrors.IsNotFound(err) {
				// The StatefulSet controller creates a missing claim from its template.
				tmpl := w.(*appsv1.StatefulSet).Spec.VolumeClaimTemplates[0]
				claim = &corev1.PersistentVolumeClaim{Name: "data-" + name, Namespace: x.f.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations, Spec: tmpl.Spec}
				claim.UID = types.UID(fmt.Sprintf("claim-%s-%d", name, x.clock.UnixNano()))
				if err := x.r.Create(ctx, claim); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if claim.Spec.VolumeName == "" {
				claim.Spec.VolumeName = "pv-" + string(claim.UID)
				if err := x.r.Update(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			claim.Status.Phase = corev1.ClaimBound
			if err := x.r.Status().Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
			pv := &corev1.PersistentVolume{Name: claim.Spec.VolumeName, UID: types.UID("volume-" + string(claim.UID)), Finalizers: []string{csiDeletionFinalizer}, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "ebs.csi.aws.com"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete, StorageClassName: x.f.Spec.Storage.StorageClassName, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-" + string(claim.UID)}}}}
			if err := x.r.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}})
		}
		if x.admit != nil {
			x.admit(&spec)
		}
		uid := types.UID(fmt.Sprintf("%s-%d", name, x.clock.UnixNano()))
		p = &corev1.Pod{Name: name, Namespace: x.f.Namespace, UID: uid, Labels: podLabels, OwnerReferences: []metav1.OwnerReference{owner}, Spec: spec, Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.0.0.%d", i+1), ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-" + string(uid), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(x.clock.Add(-time.Hour))}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		if err := x.r.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		w.Status.ReadyReplicas = n
		w.Status.Replicas = n
		w.Status.ObservedGeneration = w.Generation
		w.Status.UpdatedReplicas = updated
		w.Status.UpdateRevision = revision
		// Like the real controller, only a completed RollingUpdate advances
		// currentRevision; OnDelete never does.
		if w.Status.CurrentRevision == "" || (w.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && updated == n) {
			w.Status.CurrentRevision = revision
		}
	case *appsv1.Deployment:
		w.Status.ReadyReplicas = n
		w.Status.Replicas = n
		w.Status.ObservedGeneration = w.Generation
		w.Status.UpdatedReplicas = n
	}
	if err := x.r.Status().Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	x.clock = x.clock.Add(time.Second)
}

// syncStorage simulates a conforming CSI provisioner, not the operator. It
// removes backend storage and releases the deletion finalizer after PVC absence.
func (x *operationFixture) syncStorage() {
	x.t.Helper()
	pvs := &corev1.PersistentVolumeList{}
	if err := x.r.List(x.t.Context(), pvs); err != nil {
		x.t.Fatal(err)
	}
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil {
			continue
		}
		c := &corev1.PersistentVolumeClaim{}
		if err := x.r.Get(x.t.Context(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, c); !apierrors.IsNotFound(err) {
			continue
		}
		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			continue
		}
		if err := x.r.Delete(x.t.Context(), &pv); err != nil {
			x.t.Fatal(err)
		}
		if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(&pv), &pv); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			x.t.Fatal(err)
		}
		pv.Finalizers = nil
		if err := x.r.Update(x.t.Context(), &pv); err != nil {
			x.t.Fatal(err)
		}
	}
}

func envReservation(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *fleet.CelldStorageReservation {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	return res
}

// The controller consumes celld's own reports; it never reads private
// recovery metadata or drives the removed strict executor.
func TestNoRecoveryMetadataDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"internal/runtime/v050", "internal/runtime/v041", "internal/runtime/catalog", "internal/fencing", "internal/launcher", "RemoveDisk", "ProductionEvidence", "lifecycleJournal"} {
			if strings.Contains(string(b), forbidden) {
				t.Errorf("%s retains removed runtime authority %s", entry.Name(), forbidden)
			}
		}
	}
}
