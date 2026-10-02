package v1alpha1

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkFleetValidation(b *testing.B) {
	for _, telemetry := range []bool{false, true} {
		b.Run(fmt.Sprintf("telemetry=%t", telemetry), func(b *testing.B) {
			fleet := valid()
			if telemetry {
				fleet.Spec.Telemetry = &TelemetrySpec{CollectorURL: "http://collector.fleets.svc:4318", Egress: CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "0.25"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := fleet.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRuntimeImageValidation(b *testing.B) {
	for name, repository := range map[string]string{
		"ghcr": "ghcr.io/ewhauser/celld",
		"ecr":  "123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld",
		"long": "registry.example.com/" + strings.Repeat("segment/", 50) + "celld",
	} {
		b.Run(name, func(b *testing.B) {
			image := repository + "@sha256:" + strings.Repeat("a", 64)
			b.ReportAllocs()
			for b.Loop() {
				if !ValidRuntimeImage(image) {
					b.Fatal("valid pinned image rejected")
				}
			}
		})
	}
}

func BenchmarkExportBucketValidation(b *testing.B) {
	fleet := valid()
	fleet.Spec.Export = &ExportSpec{Bucket: &ExportBucketSpec{Name: "change-export"}}
	b.ReportAllocs()
	for b.Loop() {
		if err := fleet.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSeedValidation(b *testing.B) {
	for _, count := range []int{1, 100} {
		b.Run(fmt.Sprintf("objects=%d", count), func(b *testing.B) {
			seed := &PreviewSeedSpec{Source: "source", Objects: make([]PreviewObjectReference, count)}
			for i := range seed.Objects {
				seed.Objects[i] = PreviewObjectReference{Class: "Counter", ID: fmt.Sprintf("cell-%d", i)}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := seed.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
