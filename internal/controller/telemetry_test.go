package controller

import (
	"slices"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestTelemetryRendersEnvAndEgress(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	before := specHash(f)
	f.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "http://collector.fleets.svc:4318", Egress: fleet.CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "0.25", FlushMilliseconds: 5000, FlushBytes: 4096, HeadersSecretKeyRef: &fleet.TelemetrySecretKeyRef{Name: "otel-auth", Key: "headers"}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if specHash(f) == before {
		t.Fatal("telemetry missing from immutable reservation hash")
	}
	env := podTemplate(f, Options{}).Spec.Containers[0].Env
	for key, want := range map[string]string{"CELLD_OTEL": "http://collector.fleets.svc:4318", "CELLD_OTEL_FLUSH_MS": "5000", "CELLD_OTEL_FLUSH_BYTES": "4096", "OTEL_TRACES_SAMPLER": "traceidratio", "OTEL_TRACES_SAMPLER_ARG": "0.25"} {
		if got, _ := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	foundHeaders := false
	for _, e := range env {
		if e.Name == "CELLD_OTEL_SINK" {
			t.Fatal("removed sink emitted")
		}
		if e.Name == "OTEL_EXPORTER_OTLP_HEADERS" {
			foundHeaders = true
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != "otel-auth" || e.ValueFrom.SecretKeyRef.Key != "headers" {
				t.Fatal("headers not secret-backed")
			}
		}
	}
	if !foundHeaders {
		t.Fatal("headers Secret reference missing")
	}
	var policy *networkingv1.NetworkPolicy
	for _, o := range prerequisites(f, Options{}) {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok {
			policy = p
		}
	}
	if policy == nil || len(policy.Spec.Egress) != 5 {
		t.Fatalf("telemetry egress missing: %+v", policy)
	}
	last := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	if len(last.To) != 1 || last.To[0].PodSelector == nil || last.To[0].PodSelector.MatchLabels["app"] != "otel" || len(last.Ports) != 1 || last.Ports[0].Port.IntValue() != 4318 {
		t.Fatalf("collector rule too broad or missing: %+v", last)
	}
	f.Spec.Telemetry = nil
	for _, o := range prerequisites(f, Options{}) {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok && len(p.Spec.Egress) != 4 {
			t.Fatalf("disabled telemetry changed egress: %+v", p.Spec.Egress)
		}
	}
	for _, e := range podTemplate(f, Options{}).Spec.Containers[0].Env {
		if slices.Contains([]string{"CELLD_OTEL", "CELLD_OTEL_SINK", "OTEL_EXPORTER_OTLP_HEADERS"}, e.Name) {
			t.Fatalf("disabled telemetry emitted %s", e.Name)
		}
	}
}

func TestTelemetryIPCollectorEgress(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "https://192.0.2.10", Egress: fleet.CollectorEgress{CIDR: "192.0.2.10/32"}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, o := range prerequisites(f, Options{}) {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok {
			last := p.Spec.Egress[len(p.Spec.Egress)-1]
			if len(last.To) != 1 || last.To[0].IPBlock == nil || last.To[0].IPBlock.CIDR != "192.0.2.10/32" || last.Ports[0].Port.IntValue() != 443 {
				t.Fatalf("IP collector egress incorrect: %+v", last)
			}
			return
		}
	}
	t.Fatal("policy missing")
}
