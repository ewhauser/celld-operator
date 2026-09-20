package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLocalRWOPUsesReadWriteOncePodAndHostpathCSIIdentity(t *testing.T) {
	opts := Options{LocalTest: true, LauncherImage: "local-launcher"}
	if modes := persistentAccessModes(opts); modes[0] != corev1.ReadWriteOnce {
		t.Fatalf("plain local test must keep ReadWriteOnce, got %v", modes)
	}
	opts.LocalRWOP = true
	if modes := persistentAccessModes(opts); modes[0] != corev1.ReadWriteOncePod {
		t.Fatalf("local RWOP must request ReadWriteOncePod, got %v", modes)
	}
	if modes := persistentAccessModes(Options{LocalRWOP: true}); modes[0] != corev1.ReadWriteOnce {
		t.Fatal("LocalRWOP without LocalTest or a launcher must have no effect")
	}
	claim := &corev1.PersistentVolumeClaim{Name: "data-x-0", Namespace: "fleets", UID: "claim", Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-x"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	csi := &corev1.PersistentVolume{Name: "pv-x", UID: "pvuid", Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: localCSIDriver, VolumeHandle: "vol-1"}}}}
	other := csi.DeepCopy()
	other.Name, other.UID = "pv-other", "pvuid-other"
	other.Spec.CSI.Driver = "someone.else.csi"
	f := fixture("x", "bucket-x", "PersistentFleet")
	claim.Labels = labels(f)
	claim.Annotations = map[string]string{"celld.eric.dev/storage-reservation": reservationName(f)}
	s := &fleetState{Claims: map[string]types.UID{claim.Name: claim.UID}}
	r := setup(t, claim, csi, other)
	r.Options.LocalTest = true
	v, err := r.targetVolume(t.Context(), f, s, "x-0")
	if err != nil || v.VolumeUID != "pvuid" || v.Handle != localCSIDriver+":vol-1" {
		t.Fatalf("hostpath CSI identity not accepted in local test: %s %s %v", v.VolumeUID, v.Handle, err)
	}
	r.Options.LocalTest = false
	if _, err := r.targetVolume(t.Context(), f, s, "x-0"); err == nil {
		t.Fatal("hostpath CSI volume accepted outside local test")
	}
	r.Options.LocalTest = true
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), claim); err != nil {
		t.Fatal(err)
	}
	claim.Spec.VolumeName = "pv-other"
	if err := r.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if _, err := r.targetVolume(t.Context(), f, s, "x-0"); err == nil {
		t.Fatal("unknown CSI driver accepted in local test")
	}
	_ = metav1.Now()
}
