package controller

import (
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestMaintenanceRetainsStorageAndLossFences(t *testing.T) {
	for _, failure := range []string{"claim", "loss"} {
		t.Run(failure, func(t *testing.T) {
			r, f := lifecycleSetup(t, "PersistentFleet")
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			j := getJournal(t, r, f)
			j.Maintenance = &maintenanceOperation{ID: "review-restart", Kind: "Restart", Token: "restart", Phase: "Capture", Deadline: time.Now().Add(time.Hour)}
			if failure == "claim" {
				for name := range j.Claims {
					j.Claims[name] = "replaced"
					break
				}
			}
			if err := r.saveJournal(t.Context(), res, j); err != nil {
				t.Fatal(err)
			}
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if failure == "loss" {
				if w.GetAnnotations() == nil {
					w.SetAnnotations(map[string]string{})
				}
				w.GetAnnotations()[lossFenceKey] = "review-loss"
				if err := r.Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.lifecycle(t.Context(), f, r.hydrate(t.Context(), res), w); err != nil {
				t.Fatal(err)
			}
			if failure == "loss" {
				if getJournal(t, r, f).Loss != "review-loss" {
					t.Fatal("maintenance bypassed durable loss mirroring")
				}
			} else {
				got := &fleet.CelldFleet{}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
					t.Fatal(err)
				}
				reason(t, got, "StorageIdentityConflict")
			}
		})
	}
}

func TestOrderedMaintenanceReleasesReplacementGate(t *testing.T) {
	f := fixture("ordered-maintenance", "ordered-maintenance", "Bucket")
	f.Spec.BucketWorkload = "Ordered"
	w := workload(f, Options{})
	w.SetUID("ordered-workload")
	pod := &corev1.Pod{Name: f.Name + "-0", Namespace: f.Namespace, Labels: labels(f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: w.GetUID(), Controller: new(true)}}, Spec: podTemplate(f, Options{}).Spec}
	res := &fleet.CelldStorageReservation{Name: reservationName(f)}
	r := setup(t, f, w, pod, res)
	j := &lifecycleJournal{Applied: f.Spec.Replicas, WorkloadUID: w.GetUID(), Maintenance: &maintenanceOperation{ID: "restart", Kind: "Restart", Token: "one", Phase: "Recovering", Targets: []maintenanceTarget{{Name: pod.Name, UID: "old-uid"}}}}
	if _, _, err := r.executeMaintenance(t.Context(), f, res, j, w); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pod), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.SchedulingGates) != 0 || got.Spec.NodeSelector[corev1.LabelTopologyZone] != f.Spec.Placement.Zones[0] {
		t.Fatal("maintenance deadlocked replacement scheduling")
	}
}
