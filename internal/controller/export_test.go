package controller

import (
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func fleetPolicy(t *testing.T, f *fleet.CelldFleet) *networkingv1.NetworkPolicy {
	t.Helper()
	for _, o := range prerequisites(f, Options{}) {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok {
			return p
		}
	}
	t.Fatal("no NetworkPolicy")
	return nil
}

func TestBucketExportRendersEnv(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	before := specHash(f)
	f.Spec.Export = &fleet.ExportSpec{Sink: "Bucket", Classes: []string{"Cart", "Order"}, ExcludeTables: []string{"Cart.audit"}, QueueBytes: 67108864, Bucket: &fleet.ExportBucketSpec{Name: "alpha-export", FlushMilliseconds: 2000, RetentionDays: 14}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if specHash(f) == before {
		t.Fatal("export missing from immutable reservation hash")
	}
	pod := podTemplate(f, Options{}).Spec
	env := pod.Containers[0].Env
	for key, want := range map[string]string{
		"CELLD_EXPORT": "1", "CELLD_EXPORT_SINK": "bucket", "CELLD_EXPORT_CLASSES": "Cart,Order", "CELLD_EXPORT_TABLES": "Cart.audit",
		"CELLD_EXPORT_QUEUE_BYTES": "67108864", "CELLD_EXPORT_BUCKET": "alpha-export", "CELLD_EXPORT_FLUSH_MS": "2000", "CELLD_EXPORT_RETENTION": "14d",
	} {
		if got, _ := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// Omitted settings keep celld's defaults.
	for _, key := range []string{"CELLD_EXPORT_FLUSH_BYTES", "CELLD_EXPORT_MAX_TX_BYTES", "CELLD_EXPORT_MAX_RECORD_BYTES", "CELLD_EXPORT_KAFKA_BROKERS", "CELLD_EXPORT_TOPIC"} {
		if _, ok := envValue(env, key); ok {
			t.Errorf("%s set without a field", key)
		}
	}
	if len(pod.Volumes) != 1 {
		t.Fatalf("bucket export added volumes: %+v", pod.Volumes)
	}
	if got := len(fleetPolicy(t, f).Spec.Egress); got != 4 {
		t.Fatalf("bucket export changed egress: %d rules", got)
	}
}

func TestKafkaExportRendersEnvSecretAndEgress(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Export = &fleet.ExportSpec{Sink: "Kafka", Kafka: &fleet.ExportKafkaSpec{
		Brokers: []string{"kafka-0.kafka:9092", "kafka-1.kafka:9092", "kafka-tls.kafka:9093"}, Topic: "cart-changes", RetryMilliseconds: 60000,
		PropertiesSecretKeyRef: &fleet.SecretKeyRef{Name: "kafka-client", Key: "client.properties"},
		Egress:                 fleet.CollectorEgress{PodLabels: map[string]string{"app": "kafka"}, Namespace: "kafka"},
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := podTemplate(f, Options{}).Spec
	env := pod.Containers[0].Env
	for key, want := range map[string]string{
		"CELLD_EXPORT": "1", "CELLD_EXPORT_SINK": "kafka", "CELLD_EXPORT_KAFKA_BROKERS": "kafka-0.kafka:9092,kafka-1.kafka:9092,kafka-tls.kafka:9093",
		"CELLD_EXPORT_TOPIC": "cart-changes", "CELLD_EXPORT_RETRY_MS": "60000", "CELLD_EXPORT_KAFKA_PROPERTIES": "/etc/celld/export/kafka.properties",
	} {
		if got, _ := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := envValue(env, "CELLD_EXPORT_BUCKET"); ok {
		t.Error("bucket sink setting rendered for Kafka")
	}
	var volume *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == exportKafkaVolume {
			volume = &pod.Volumes[i]
		}
	}
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "kafka-client" || len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Key != "client.properties" || volume.Secret.Items[0].Path != "kafka.properties" {
		t.Fatalf("properties Secret not mounted: %+v", pod.Volumes)
	}
	mounted := false
	for _, m := range pod.Containers[0].VolumeMounts {
		mounted = mounted || (m.Name == exportKafkaVolume && m.MountPath == "/etc/celld/export" && m.ReadOnly)
	}
	if !mounted {
		t.Fatalf("properties not mounted read-only: %+v", pod.Containers[0].VolumeMounts)
	}
	policy := fleetPolicy(t, f)
	last := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	if len(policy.Spec.Egress) != 5 || len(last.To) != 1 || last.To[0].PodSelector.MatchLabels["app"] != "kafka" || last.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kafka" {
		t.Fatalf("broker rule missing or too broad: %+v", policy.Spec.Egress)
	}
	if len(last.Ports) != 2 || last.Ports[0].Port.IntValue() != 9092 || last.Ports[1].Port.IntValue() != 9093 {
		t.Fatalf("broker ports = %+v", last.Ports)
	}
}

func TestNoExportRendersNothing(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	for _, e := range podTemplate(f, Options{}).Spec.Containers[0].Env {
		if len(e.Name) >= 12 && e.Name[:12] == "CELLD_EXPORT" {
			t.Fatalf("%s rendered without spec.export", e.Name)
		}
	}
}
