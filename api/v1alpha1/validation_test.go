package v1alpha1

import (
	"strings"
	"testing"
)

func valid() *CelldFleet {
	f := &CelldFleet{
		Name: "test",
		Spec: CelldFleetSpec{
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
		"mutable tag":  func(f *CelldFleet) { f.Spec.RuntimeImage = "ghcr.io/denoland/celld:v0.5.0" },
		"short digest": func(f *CelldFleet) { f.Spec.RuntimeImage = "example.org/celld@sha256:abc" },
		"no registry":  func(f *CelldFleet) { f.Spec.RuntimeImage = "celld@sha256:" + strings.Repeat("a", 64) },
		"uppercase digest": func(f *CelldFleet) {
			f.Spec.RuntimeImage = "mirror.example.com/celld@sha256:" + strings.Repeat("A", 64)
		},
		"embedded tag": func(f *CelldFleet) {
			f.Spec.RuntimeImage = "mirror.example.com/celld:v1@sha256:" + strings.Repeat("a", 64)
		},
		"long token":      func(f *CelldFleet) { f.Spec.Maintenance = &MaintenanceSpec{RestartToken: strings.Repeat("a", 129)} },
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

func TestMirroredRuntimeImage(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/ewhauser/celld@sha256:" + strings.Repeat("a", 64),
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld@sha256:" + strings.Repeat("b", 64),
		"localhost:5000/team/celld@sha256:" + strings.Repeat("c", 64),
	} {
		f := valid()
		f.Spec.RuntimeImage = image
		if err := f.Validate(); err != nil {
			t.Errorf("%s: %v", image, err)
		}
	}
}

func TestAdditionalEnvironmentValidation(t *testing.T) {
	value := "debug"
	validEntries := []FleetEnvVar{
		{Name: "CELLD_LOG", Value: &value},
		{Name: "CELLD_API_TOKEN", SecretKeyRef: &SecretKeyRef{Name: "runtime-auth", Key: "token"}},
	}
	f := valid()
	f.Spec.Env = validEntries
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]FleetEnvVar{
		"override bucket":   {{Name: "CELLD_BUCKET", Value: &value}},
		"override launcher": {{Name: "CELLD_REEXEC_PROBE_SIGNING_KEY", Value: &value}},
		"override otel":     {{Name: "CELLD_OTEL", Value: &value}},
		"duplicate":         {{Name: "CELLD_LOG", Value: &value}, {Name: "CELLD_LOG", Value: &value}},
		"both sources":      {{Name: "CELLD_LOG", Value: &value, SecretKeyRef: &SecretKeyRef{Name: "s", Key: "k"}}},
		"no source":         {{Name: "CELLD_LOG"}},
		"bad key":           {{Name: "CELLD_LOG", SecretKeyRef: &SecretKeyRef{Name: "s", Key: "bad/key"}}},
		"unrelated env":     {{Name: "AWS_REGION", Value: &value}},
	} {
		t.Run(name, func(t *testing.T) {
			f := valid()
			f.Spec.Env = entries
			if err := f.Validate(); err == nil {
				t.Fatal("accepted invalid environment")
			}
		})
	}
}

func TestTelemetryValidation(t *testing.T) {
	f := valid()
	f.Spec.Telemetry = &TelemetrySpec{CollectorURL: "http://collector.fleets.svc:4318", Egress: CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "0.25", FlushMilliseconds: 5000, FlushBytes: 4096, HeadersSecretKeyRef: &TelemetrySecretKeyRef{Name: "otel-auth", Key: "headers"}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*TelemetrySpec){
		"missing egress": func(s *TelemetrySpec) { s.Egress = CollectorEgress{} },
		"broad CIDR":     func(s *TelemetrySpec) { s.Egress = CollectorEgress{CIDR: "10.0.0.0/8"} },
		"query":          func(s *TelemetrySpec) { s.CollectorURL += "?token=secret" },
		"credentials":    func(s *TelemetrySpec) { s.CollectorURL = "https://user:pass@collector.example.com" },
		"bad sampler":    func(s *TelemetrySpec) { s.Sampler = "unknown" },
		"bad ratio":      func(s *TelemetrySpec) { s.SamplerArg = "2.0" },
		"missing ratio":  func(s *TelemetrySpec) { s.SamplerArg = "" },
		"bad secret":     func(s *TelemetrySpec) { s.HeadersSecretKeyRef.Key = "bad/key" },
	} {
		t.Run(name, func(t *testing.T) {
			f := valid()
			f.Spec.Telemetry = &TelemetrySpec{CollectorURL: "http://collector.fleets.svc:4318", Egress: CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "0.25", HeadersSecretKeyRef: &TelemetrySecretKeyRef{Name: "otel-auth", Key: "headers"}}
			edit(f.Spec.Telemetry)
			if err := f.Validate(); err == nil {
				t.Fatal("accepted invalid telemetry")
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
