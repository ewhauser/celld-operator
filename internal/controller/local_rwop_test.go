package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestLocalRWOPUsesReadWriteOncePodAndHostpathCSI(t *testing.T) {
	if modes := persistentAccessModes(Options{}); modes[0] != corev1.ReadWriteOncePod {
		t.Fatalf("production claims must be ReadWriteOncePod, got %v", modes)
	}
	opts := Options{LocalTest: true}
	if modes := persistentAccessModes(opts); modes[0] != corev1.ReadWriteOnce {
		t.Fatalf("plain local test must keep ReadWriteOnce, got %v", modes)
	}
	opts.LocalRWOP = true
	if modes := persistentAccessModes(opts); modes[0] != corev1.ReadWriteOncePod {
		t.Fatalf("local RWOP must request ReadWriteOncePod, got %v", modes)
	}
	r := &Reconciler{Options: Options{LocalTest: true}}
	if !r.supportedCSI(localCSIDriver) || !r.supportedCSI("ebs.csi.aws.com") || r.supportedCSI("someone.else.csi") {
		t.Fatal("local test accepts only EBS and the hostpath driver")
	}
	r.Options.LocalTest = false
	if r.supportedCSI(localCSIDriver) {
		t.Fatal("hostpath CSI accepted outside local test")
	}
}
