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
	for _, name := range []string{"CELLD_MAX_RESIDENT_CELLS", "CELLD_IDLE_EVICT_S", "LAUNCHER_TERMINATION_GRACE_SECONDS"} {
		if _, has := envValue(c.Env, name); has {
			t.Fatalf("%s must be absent when tuning is omitted", name)
		}
	}
}

func TestAdditionalEnvironmentTemplateAndHash(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	baseline := specHash(f)
	value := "debug"
	f.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_LOG", Value: &value}, {Name: "CELLD_API_TOKEN", SecretKeyRef: &fleet.SecretKeyRef{Name: "runtime-auth", Key: "token"}}}
	if specHash(f) == baseline {
		t.Fatal("environment missing from immutable reservation hash")
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	env := podTemplate(f, Options{}).Spec.Containers[0].Env
	if got, _ := envValue(env, "CELLD_LOG"); got != "debug" {
		t.Fatalf("literal env = %q", got)
	}
	for _, item := range env {
		if item.Name == "CELLD_API_TOKEN" {
			if item.Value != "" || item.ValueFrom == nil || item.ValueFrom.SecretKeyRef == nil || item.ValueFrom.SecretKeyRef.Name != "runtime-auth" || item.ValueFrom.SecretKeyRef.Key != "token" {
				t.Fatalf("secret reference missing: %+v", item)
			}
			return
		}
	}
	t.Fatal("secret reference missing")
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
	want := map[string]string{"CELLD_MAX_RESIDENT_CELLS": "400", "CELLD_IDLE_EVICT_S": "60", "CELLD_SHUTDOWN_TOTAL_MS": "120000", "LAUNCHER_TERMINATION_GRACE_SECONDS": "180"}
	for name, value := range want {
		if got, _ := envValue(c.Env, name); got != value {
			t.Fatalf("%s=%q, want %q", name, got, value)
		}
	}
	if *pod.TerminationGracePeriodSeconds != 180 {
		t.Fatalf("grace not applied: %d", *pod.TerminationGracePeriodSeconds)
	}
	// Bucket runs celld directly; its grace bounds celld's own SIGTERM drain.
	b := fixture("beta", "bucket-beta", "Bucket")
	b.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 60, TerminationGraceSeconds: 90}
	bp := podTemplate(b, opts).Spec
	if _, has := envValue(bp.Containers[0].Env, "LAUNCHER_TERMINATION_GRACE_SECONDS"); has || len(bp.InitContainers) != 0 || bp.Containers[0].Command != nil {
		t.Fatal("Bucket must not run under the launcher")
	}
	if v, _ := envValue(bp.Containers[0].Env, "CELLD_SHUTDOWN_TOTAL_MS"); v != "60000" {
		t.Fatal("Bucket drain bound not applied")
	}
	if *bp.TerminationGracePeriodSeconds != 90 {
		t.Fatal("Bucket grace not applied")
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
