package controller

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestOrderedBucketZoneGateAndDeterministicVictim(t *testing.T) {
	f := fixture("ordered", "ordered-data", "Bucket")
	f.Spec.BucketWorkload = "Ordered"
	w := workload(f, Options{}).(*appsv1.StatefulSet)
	// Maintenance stops every child before scaling to zero. OrderedReady would
	// wait for a lower unready ordinal and never delete the higher stopped pod.
	if len(w.Spec.VolumeClaimTemplates) != 0 || w.Spec.PodManagementPolicy != appsv1.ParallelPodManagement || len(w.Spec.Template.Spec.TopologySpreadConstraints) != 0 {
		t.Fatal("ordered Bucket must use ephemeral disk and ordinal placement")
	}
	objects := []client.Object{}
	for i := range 3 {
		name := fmt.Sprintf("ordered-%d", i)
		pod := &corev1.Pod{Name: name, Namespace: f.Namespace, UID: types.UID(name), Labels: labels(f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: "workload", Controller: new(true)}}, Spec: *w.Spec.Template.Spec.DeepCopy()}
		objects = append(objects, pod)
	}
	r := setup(t, objects...)
	j := &fleetState{WorkloadUID: "workload"}
	if err := r.scheduleOrderedBucket(t.Context(), f, j); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		pod := &corev1.Pod{}
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: fmt.Sprintf("ordered-%d", i)}, pod); err != nil {
			t.Fatal(err)
		}
		zone := f.Spec.Placement.Zones[i%2]
		if len(pod.Spec.SchedulingGates) != 0 || pod.Spec.NodeSelector[corev1.LabelTopologyZone] != zone {
			t.Fatal("ordinal escaped assigned zone")
		}
	}

}

func TestOrderedBucketGateRejectsForeignAndConflictingPods(t *testing.T) {
	for _, change := range []string{"owner", "zone", "ordinal", "scheduled"} {
		t.Run(change, func(t *testing.T) {
			f := fixture("ordered", "ordered-data", "Bucket")
			f.Spec.BucketWorkload = "Ordered"
			pod := &corev1.Pod{Name: "ordered-0", Namespace: f.Namespace, Labels: labels(f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: "workload", Controller: new(true)}}, Spec: podTemplate(f, Options{}).Spec}
			switch change {
			case "owner":
				pod.OwnerReferences[0].UID = "other"
			case "zone":
				pod.Spec.NodeSelector = map[string]string{corev1.LabelTopologyZone: "us-east-1b"}
			case "ordinal":
				pod.Name = "ordered-00"
			case "scheduled":
				pod.Spec.NodeName = "node"
			}
			r := setup(t, pod)
			if err := r.scheduleOrderedBucket(t.Context(), f, &fleetState{WorkloadUID: "workload"}); err == nil {
				t.Fatal("unsafe gate release")
			}
			got := &corev1.Pod{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(pod), got); err != nil {
				t.Fatal(err)
			}
			if len(got.Spec.SchedulingGates) != 1 {
				t.Fatal("failed admission removed gate")
			}
		})
	}
}

func TestOrderedBucketDoesNotChangeLegacyReservationHash(t *testing.T) {
	f := fixture("legacy", "legacy-data", "Bucket")
	f.Spec.BucketWorkload = ""
	old := specHash(f)
	f.Default()
	if specHash(f) != old {
		t.Fatal("default invalidated legacy storage reservation")
	}
	f.Spec.BucketWorkload = "Ordered"
	if specHash(f) == old {
		t.Fatal("layout change silently adopted storage")
	}
}

func TestOrderedBucketReconcileProvisionAndExpandWithoutClaims(t *testing.T) {
	f := fixture("ordered", "ordered-data", "Bucket")
	f.Spec.BucketWorkload = "Ordered"
	f.Spec.Replicas = 2
	r := setup(t, f)
	reconcile(t, r, f)
	w := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	w.UID = "ordered-sts"
	if err := r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	f = reconcile(t, r, f)
	f.Spec.Replicas = 3
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		reconcile(t, r, f)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if replicas(w) != 3 {
		t.Fatalf("ordered expansion did not execute: %+v", reconcile(t, r, f).Status.Conditions)
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(t.Context(), claims); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatal("ordered Bucket created persistent claims")
	}
}
