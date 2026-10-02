package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Keep the complete reconcile benchmark separate from CPU profiles of individual
// helpers: the fake API's JSON serialization contributes to its timings.
func performanceFixture(tb testing.TB, profile string, n int32) (*Reconciler, *fleet.CelldFleet, client.Object) {
	tb.Helper()
	f := fixture("perf", "bucket-perf", profile)
	f.Spec.Replicas = n
	f.Finalizers = []string{Finalizer}
	f.Generation = 1
	opts := Options{OperatorNamespace: "celld-system"}
	w := workload(f, opts)
	w.SetUID("workload")
	w.SetGeneration(1)
	objects := []client.Object{f, w, &corev1.ServiceAccount{Name: "runtime", Namespace: f.Namespace},
		&storagev1.StorageClass{Name: "disposable", Provisioner: "ebs.csi.aws.com", ReclaimPolicy: new(corev1.PersistentVolumeReclaimDelete), VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer)},
		&fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)}}
	objects = append(objects, prerequisites(f, opts)...)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	switch w := w.(type) {
	case *appsv1.Deployment:
		w.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n}
		objects = append(objects, &appsv1.ReplicaSet{Name: "perf-rs", Namespace: f.Namespace, UID: "rs", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: f.Name, UID: w.UID, Controller: new(true)}}})
	case *appsv1.StatefulSet:
		w.Status = appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: n, UpdatedReplicas: n, ReadyReplicas: n, CurrentRevision: "r1", UpdateRevision: "r1"}
	}
	for ordinal := range n {
		owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: w.GetUID(), Controller: new(true)}
		if _, ok := w.(*appsv1.Deployment); ok {
			owner.Kind, owner.Name, owner.UID = "ReplicaSet", "perf-rs", "rs"
		}
		p := &corev1.Pod{Name: memberName(f, ordinal), Namespace: f.Namespace, UID: types.UID(fmt.Sprintf("pod-%d", ordinal)), Labels: labels(f), OwnerReferences: []metav1.OwnerReference{owner},
			Spec:   corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "celld", Image: f.Spec.RuntimeImage}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}}}}}
		objects = append(objects, p)
		if profile == "PersistentFleet" {
			objects = append(objects, &corev1.PersistentVolumeClaim{Name: claimName(f, ordinal), Namespace: f.Namespace, UID: types.UID(fmt.Sprintf("claim-%d", ordinal)), Labels: labels(f), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}})
		}
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		tb.Fatal(err)
	}
	if err := fleet.AddToScheme(scheme); err != nil {
		tb.Fatal(err)
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&fleet.CelldFleet{}, &appsv1.Deployment{}, &appsv1.StatefulSet{}).WithObjects(objects...).Build(), Options: opts, NetworkPolicyEnforced: true, now: func() time.Time { return now }}
	return r, f, w
}

func BenchmarkFleetReconcile(b *testing.B) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, n := range []int32{3, 100} {
			b.Run(fmt.Sprintf("%s/%d", profile, n), func(b *testing.B) {
				r, f, _ := performanceFixture(b, profile, n)
				ctx := b.Context()
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}
				if _, err := r.Reconcile(ctx, req); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := r.Reconcile(ctx, req); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkCurrentPods(b *testing.B) {
	r, f, w := performanceFixture(b, "Bucket", 100)
	ctx := b.Context()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.currentPods(ctx, f, w.GetUID()); err != nil {
			b.Fatal(err)
		}
	}
}

type performanceCollector struct{ observation capacity.Observation }

func (c performanceCollector) Collect(context.Context, *fleet.CelldFleet) capacity.Observation {
	return c.observation
}

func BenchmarkFleetContraction(b *testing.B) {
	r, f, w := performanceFixture(b, "Bucket", 100)
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "External"}
	f.Spec.Capacity.Default()
	o := capacity.Observation{At: r.capacityNow(), Complete: true}
	pods, err := r.currentPods(b.Context(), f, w.GetUID())
	if err != nil {
		b.Fatal(err)
	}
	for i := range pods {
		id, _ := podIdentity(&pods[i])
		o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 1, MemoryMiB: 1, RuntimeMemoryMiB: 1, RuntimeAt: o.At, RuntimeReceived: o.At, MetricsAt: o.At, MetricsReceived: o.At, Window: 15 * time.Second})
	}
	r.Collector = performanceCollector{o}
	desired := workload(f, r.Options)
	ctx := b.Context()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.contraction(ctx, f, desired, w, false); err != nil {
			b.Fatal(err)
		}
	}
}

func donorObservation() (fleet.CapacityPolicy, capacity.Observation, []string) {
	p := fleet.CapacityPolicy{}
	p.Default()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := capacity.Observation{At: now, Complete: true}
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("member-%d", i)
		o.Samples = append(o.Samples, capacity.Sample{Identity: ids[i], Ready: true, CPU: 1, MemoryMiB: 1, RuntimeMemoryMiB: 1, RuntimeAt: now, RuntimeReceived: now, MetricsAt: now, MetricsReceived: now, Window: 15 * time.Second})
	}
	return p, o, ids
}

func BenchmarkValidateDonors(b *testing.B) {
	p, o, ids := donorObservation()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := validateDonors(p, o, ids, ids, o.At); err != nil {
			b.Fatal(err)
		}
	}
}

func TestValidateDonorsPreservesPerVictimChecks(t *testing.T) {
	for _, scenario := range []string{"healthy", "incomplete", "future", "stale", "duplicate", "foreign", "unready", "pressure", "backlog", "short-window", "cpu-overflow", "runtime-memory-overflow", "missing-donor"} {
		t.Run(scenario, func(t *testing.T) {
			p, o, ids := donorObservation()
			now := o.At
			donors := ids
			switch scenario {
			case "incomplete":
				o.Complete = false
			case "future":
				o.At = now.Add(time.Second)
			case "stale":
				now = now.Add(capacity.Seconds(p.MaxAgeSeconds) + time.Second)
			case "duplicate":
				o.Samples[99].Identity = ids[0]
			case "foreign":
				o.Samples[99].Identity = "replacement"
			case "unready":
				o.Samples[99].Ready = false
			case "pressure":
				o.Samples[99].Pressured = true
			case "backlog":
				o.Samples[99].Backlog = true
			case "short-window":
				o.Samples[99].Window = time.Nanosecond
			case "cpu-overflow":
				p.CPUHighMillicores, p.CPULowMillicores = 1000, 900
				o.Samples[0].CPU, o.Samples[99].CPU = 800, 800
			case "runtime-memory-overflow":
				o.Samples[99].RuntimeMemoryMiB = int64(p.MemoryHighMiB)
			case "missing-donor":
				donors = []string{"absent"}
			}
			var individual error
			for _, donor := range donors {
				if individual = ValidateSurvivors(p, o, ids, donor, now); individual != nil {
					break
				}
			}
			combined := validateDonors(p, o, ids, donors, now)
			if (combined == nil) != (individual == nil) {
				t.Fatalf("combined=%v individual=%v", combined, individual)
			}
			if (combined == nil) != (scenario == "healthy") {
				t.Fatalf("scenario %s accepted=%t error=%v", scenario, combined == nil, combined)
			}
		})
	}
}

func TestCurrentPodsReadsEachReplicaSetOnce(t *testing.T) {
	r, f, w := performanceFixture(t, "Bucket", 100)
	reads := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*appsv1.ReplicaSet); ok {
			reads++
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	if pods, err := r.currentPods(t.Context(), f, w.GetUID()); err != nil || len(pods) != 100 {
		t.Fatalf("pods=%d err=%v", len(pods), err)
	}
	if reads != 1 {
		t.Fatalf("read the shared ReplicaSet %d times, want 1", reads)
	}
	// Reuse must still validate every Pod's owner UID, and stop at this call's
	// boundary: a ReplicaSet replacement must be seen on the next call.
	p := &corev1.Pod{}
	key := client.ObjectKey{Namespace: f.Namespace, Name: memberName(f, 99)}
	if err := r.Get(t.Context(), key, p); err != nil {
		t.Fatal(err)
	}
	p.OwnerReferences[0].UID = "different-rs"
	if err := r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.currentPods(t.Context(), f, w.GetUID()); err == nil {
		t.Fatal("accepted a Pod referencing a different ReplicaSet UID")
	}
	p.OwnerReferences[0].UID = "rs"
	if err := r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: "perf-rs"}, rs); err != nil {
		t.Fatal(err)
	}
	rs.OwnerReferences[0].UID = "different-workload"
	if err := r.Update(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
	if _, err := r.currentPods(t.Context(), f, w.GetUID()); err == nil {
		t.Fatal("reused stale ReplicaSet ownership across calls")
	}
}

func TestSteadyFleetDoesNotCreateExistingReservation(t *testing.T) {
	r, f, _ := performanceFixture(t, "Bucket", 3)
	creates := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok {
			creates++
		}
		return c.Create(ctx, obj, opts...)
	}})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
		t.Fatal(err)
	}
	if creates != 0 {
		t.Fatalf("attempted %d writes to an existing permanent reservation", creates)
	}
}

func TestReservationCreationRaceStillChecksWinner(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t", foreign), func(t *testing.T) {
			r, f, _ := performanceFixture(t, "Bucket", 3)
			base := r.Client.(client.WithWatch)
			raced := false
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*fleet.CelldStorageReservation); ok && !raced {
					raced = true
					if foreign {
						winner := &fleet.CelldStorageReservation{}
						if err := c.Get(ctx, key, winner); err != nil {
							return err
						}
						// The fake client permits immutable-spec edits; this stands
						// in for a different winner at the atomic Create boundary.
						winner.Spec.FleetUID = "foreign"
						if err := c.Update(ctx, winner); err != nil {
							return err
						}
					}
					return apierrors.NewNotFound(schema.GroupResource{Group: "celld.eric.dev", Resource: "celldstoragereservations"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
				t.Fatal(err)
			}
			got := &fleet.CelldFleet{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
				t.Fatal(err)
			}
			if foreign {
				reason(t, got, "StorageScopeConflict")
			} else {
				reason(t, got, "Provisioned")
			}
		})
	}
}
