package v1alpha1

import "testing"

func TestCapacityDefaultsAndValidation(t *testing.T) {
	p := &CapacityPolicy{}
	p.Default()
	if p.Mode != "Shadow" || p.MinReplicas != 3 || p.MaxReplicas != 10 || p.Validate() != nil {
		t.Fatal(p)
	}
	for _, mutate := range []func(*CapacityPolicy){
		func(p *CapacityPolicy) { p.Mode = "Enabled" }, func(p *CapacityPolicy) { p.MaxReplicas = 101 }, func(p *CapacityPolicy) { p.MinReplicas = 11 }, func(p *CapacityPolicy) { p.ScaleOutStep = 0 }, func(p *CapacityPolicy) { p.SampleIntervalSeconds = 300 }, func(p *CapacityPolicy) { p.MaxAgeSeconds = -1 }, func(p *CapacityPolicy) { p.MinWindowSeconds = 60; p.MaxWindowSeconds = 5 }, func(p *CapacityPolicy) { p.MinSamples = 1 }, func(p *CapacityPolicy) { p.ScaleInStabilizationSeconds = 1 }, func(p *CapacityPolicy) { p.CPULowMillicores = p.CPUHighMillicores }, func(p *CapacityPolicy) { p.MemoryLowMiB = p.MemoryHighMiB },
	} {
		bad := *p
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid policy accepted", bad)
		}
	}
}
