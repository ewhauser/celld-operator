package v1alpha1

import (
	"strings"
	"testing"
)

func valid() *CelldFleet {
	f := &CelldFleet{
		Name: "test",
		Spec: CelldFleetSpec{
			Qualification:      "Experimental",
			Profile:            "Bucket",
			ServiceAccountName: "runtime",
			Storage:            StorageSpec{Bucket: "example-bucket", Region: "us-east-1"},
			Placement:          PlacementSpec{AZCount: 2, Zones: []string{"us-east-1a", "us-east-1b"}},
		},
	}
	f.Default()
	return f
}
func TestDefaultsAndValidation(t *testing.T) {
	f := valid()
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if f.Spec.Replicas != 3 || f.Spec.Storage.SizeGiB != 10 || f.Spec.Placement.Mode != "Strict" {
		t.Fatal("incorrect defaults")
	}
	tests := map[string]func(*CelldFleet){
		"mutable tag":     func(f *CelldFleet) { f.Spec.RuntimeImage = "ghcr.io/denoland/celld:v0.5.0" },
		"foreign image":   func(f *CelldFleet) { f.Spec.RuntimeImage = "example.org/celld@sha256:abc" },
		"long token":      func(f *CelldFleet) { f.Spec.Maintenance = &MaintenanceSpec{RestartToken: strings.Repeat("a", 129)} },
		"production":      func(f *CelldFleet) { f.Spec.Qualification = "Production" },
		"prefix":          func(f *CelldFleet) { f.Spec.Storage.Bucket = "example-bucket/shared" },
		"alias":           func(f *CelldFleet) { f.Spec.Storage.Bucket = "EXAMPLE-bucket" },
		"profile":         func(f *CelldFleet) { f.Spec.Profile = "Unknown" },
		"storage class":   func(f *CelldFleet) { f.Spec.Profile = "PersistentFleet" },
		"bucket pvc":      func(f *CelldFleet) { f.Spec.Storage.StorageClassName = "ebs" },
		"replica floor":   func(f *CelldFleet) { f.Spec.Replicas = 1 },
		"az count":        func(f *CelldFleet) { f.Spec.Placement.AZCount = 3 },
		"duplicate zones": func(f *CelldFleet) { f.Spec.Placement.Zones[1] = "us-east-1a" },
		"foreign region":  func(f *CelldFleet) { f.Spec.Placement.Zones[1] = "us-west-2a" },
		"sa":              func(f *CelldFleet) { f.Spec.ServiceAccountName = "" },
		"name":            func(f *CelldFleet) { f.Name = "has.dot" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := valid()
			mutate(f)
			if f.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestTuningValidation(t *testing.T) {
	good := valid()
	good.Spec.Execution = &ExecutionSpec{CPURequest: "500m", CPULimit: "1", MemoryRequest: "1Gi", MemoryLimit: "2Gi", MaxResidentCells: 400, IdleEvictSeconds: 60}
	good.Spec.Lifecycle = &LifecycleSpec{ShutdownSeconds: 60, TerminationGraceSeconds: 65}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	e := good.Spec.EffectiveExecution()
	if e.CPULimit != "1" || e.MemoryLimit != "2Gi" {
		t.Fatalf("effective execution lost values: %+v", e)
	}
	if d := valid().Spec.EffectiveLifecycle(); d.ShutdownSeconds != 20 || d.TerminationGraceSeconds != 30 {
		t.Fatalf("defaults changed: %+v", d)
	}
	partial := valid()
	partial.Spec.Execution = &ExecutionSpec{MemoryLimit: "3Gi"}
	if err := partial.Validate(); err != nil {
		t.Fatalf("partial tuning must fall back to defaults: %v", err)
	}
	if e := partial.Spec.EffectiveExecution(); e.CPURequest != DefaultCPURequest || e.MemoryRequest != DefaultMemoryRequest || e.MemoryLimit != "3Gi" {
		t.Fatalf("partial merge wrong: %+v", e)
	}
	bad := map[string]func(*CelldFleet){
		"cpu limit below request":    func(f *CelldFleet) { f.Spec.Execution = &ExecutionSpec{CPURequest: "2", CPULimit: "1"} },
		"memory limit below request": func(f *CelldFleet) { f.Spec.Execution = &ExecutionSpec{MemoryRequest: "2Gi", MemoryLimit: "1Gi"} },
		"memory limit below default": func(f *CelldFleet) { f.Spec.Execution = &ExecutionSpec{MemoryLimit: "256Mi"} },
		"malformed quantity":         func(f *CelldFleet) { f.Spec.Execution = &ExecutionSpec{CPURequest: "two"} },
		"zero request":               func(f *CelldFleet) { f.Spec.Execution = &ExecutionSpec{CPURequest: "0"} },
		"grace too close to shutdown": func(f *CelldFleet) {
			f.Spec.Lifecycle = &LifecycleSpec{ShutdownSeconds: 60, TerminationGraceSeconds: 62}
		},
		"shutdown alone too long": func(f *CelldFleet) { f.Spec.Lifecycle = &LifecycleSpec{ShutdownSeconds: 40} },
		"grace alone too short":   func(f *CelldFleet) { f.Spec.Lifecycle = &LifecycleSpec{TerminationGraceSeconds: 20} },
		"shutdown beyond bound": func(f *CelldFleet) {
			f.Spec.Lifecycle = &LifecycleSpec{ShutdownSeconds: 4000, TerminationGraceSeconds: 4005}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			f := valid()
			mutate(f)
			if f.Validate() == nil {
				t.Fatal("invalid tuning accepted")
			}
		})
	}
}
