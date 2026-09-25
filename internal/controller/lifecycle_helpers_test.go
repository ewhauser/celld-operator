package controller

import (
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func getCurrentState(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *fleetState {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j, err := readState(res)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func desiredCount(t *testing.T, r *Reconciler, f *fleet.CelldFleet, n int32) *fleet.CelldFleet {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = n
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	return got
}
func lifecycleSetup(t *testing.T, profile string) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", profile)
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	reconcile(t, r, f)
	reconcile(t, r, f)
	return r, f
}

const fixtureRuntime = "ghcr.io/ewhauser/celld@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
