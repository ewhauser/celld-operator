package controller

import (
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// Omitted tuning must reproduce the historical template exactly: existing
// fleets are compared against it and a change would read as drift.
func TestTuningDefaultsPreserveHistoricalTemplate(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	opts := Options{OperatorNamespace: "celld-system", LauncherImage: "launcher@sha256:" + "a"}
	pod := podTemplate(f, opts).Spec
	c := pod.Containers[0]
	if c.Resources.Requests.Cpu().String() != "250m" || c.Resources.Requests.Memory().String() != "512Mi" || c.Resources.Limits.Memory().String() != "1Gi" {
		t.Fatalf("default resources changed: %+v", c.Resources)
	}
	if _, has := c.Resources.Limits[corev1.ResourceCPU]; has {
		t.Fatal("default template must not carry a CPU limit")
	}
	if *pod.TerminationGracePeriodSeconds != 30 {
		t.Fatalf("default grace changed: %d", *pod.TerminationGracePeriodSeconds)
	}
	if v, _ := envValue(c.Env, "CELLD_SHUTDOWN_TOTAL_MS"); v != "20000" {
		t.Fatalf("default shutdown changed: %s", v)
	}
	for _, name := range []string{"CELLD_MAX_RESIDENT_CELLS", "CELLD_IDLE_EVICT_S", "LAUNCHER_STOP_GRACE_SECONDS"} {
		if _, has := envValue(c.Env, name); has {
			t.Fatalf("%s must be absent when tuning is omitted", name)
		}
	}
}

func TestTuningIsAppliedToTheTemplate(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Execution = &fleet.ExecutionSpec{CPURequest: "2", CPULimit: "2", MemoryRequest: "4Gi", MemoryLimit: "4Gi", MaxResidentCells: 400, IdleEvictSeconds: 60}
	f.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 120, TerminationGraceSeconds: 180}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	opts := Options{OperatorNamespace: "celld-system", LauncherImage: "launcher@sha256:" + "a"}
	pod := podTemplate(f, opts).Spec
	c := pod.Containers[0]
	if c.Resources.Requests.Cpu().String() != "2" || c.Resources.Limits.Cpu().String() != "2" || c.Resources.Requests.Memory().String() != "4Gi" || c.Resources.Limits.Memory().String() != "4Gi" {
		t.Fatalf("resources not applied: %+v", c.Resources)
	}
	want := map[string]string{"CELLD_MAX_RESIDENT_CELLS": "400", "CELLD_IDLE_EVICT_S": "60", "CELLD_SHUTDOWN_TOTAL_MS": "120000", "LAUNCHER_STOP_GRACE_SECONDS": "175"}
	for name, value := range want {
		if got, _ := envValue(c.Env, name); got != value {
			t.Fatalf("%s=%q, want %q", name, got, value)
		}
	}
	if *pod.TerminationGracePeriodSeconds != 180 {
		t.Fatalf("grace not applied: %d", *pod.TerminationGracePeriodSeconds)
	}
	// Bucket keeps the emptyDir sizing from storage and never gets launcher settings.
	b := fixture("beta", "bucket-beta", "Bucket")
	b.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 60, TerminationGraceSeconds: 90}
	bp := podTemplate(b, opts).Spec
	if _, has := envValue(bp.Containers[0].Env, "LAUNCHER_STOP_GRACE_SECONDS"); has {
		t.Fatal("Bucket template carries launcher settings")
	}
	if *bp.TerminationGracePeriodSeconds != 90 {
		t.Fatal("Bucket grace not applied")
	}
	// The v0.4.1 drain-token wait tracks the shutdown budget.
	f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:ce8bbc3c26a16c9ee00e3ce0501f36bfea2663b5af8285a08fc16a54568060a5"
	if v, _ := envValue(podTemplate(f, opts).Spec.Containers[0].Env, "CELLD_DRAIN_TOKEN_WAIT_MS"); v != "90000" {
		t.Fatalf("drain wait not derived from shutdown budget: %s", v)
	}
}

// Tuning is part of the reservation's immutable spec hash for new fleets, and
// absent tuning hashes exactly as before so existing reservations still match.
func TestTuningHashStableWhenOmitted(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	before := specHash(f)
	f.Spec.Execution = nil
	f.Spec.Lifecycle = nil
	if specHash(f) != before {
		t.Fatal("nil tuning changed the hash")
	}
	f.Spec.Execution = &fleet.ExecutionSpec{CPURequest: "1"}
	if specHash(f) == before {
		t.Fatal("tuning must bind the reservation hash")
	}
}
