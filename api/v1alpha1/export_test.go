package v1alpha1

import "testing"

func kafkaExport() *ExportSpec {
	return &ExportSpec{Sink: "Kafka", Kafka: &ExportKafkaSpec{Brokers: []string{"kafka-0.kafka:9092", "kafka-1.kafka:9092"}, Egress: CollectorEgress{CIDR: "10.20.0.0/16"}}}
}

func TestExportValidation(t *testing.T) {
	for name, export := range map[string]*ExportSpec{
		"bucket defaults": {},
		"bucket tuned":    {Classes: []string{"Cart", "Order"}, ExcludeTables: []string{"Cart.audit"}, QueueBytes: 1 << 20, MaxRecordBytes: 1 << 20, Bucket: &ExportBucketSpec{Name: "acme-export", FlushMilliseconds: 1000, RetentionDays: 7}},
		"kafka":           kafkaExport(),
		"kafka pods":      {Sink: "Kafka", Kafka: &ExportKafkaSpec{Brokers: []string{"[fd00::1]:9093"}, Topic: "celld.changes", Egress: CollectorEgress{PodLabels: map[string]string{"app": "kafka"}, Namespace: "kafka"}, PropertiesSecretKeyRef: &SecretKeyRef{Name: "kafka-client", Key: "kafka.properties"}}},
	} {
		f := valid()
		f.Spec.Export = export
		f.Default()
		if err := f.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*ExportSpec){
		"queue class":       func(e *ExportSpec) { e.Classes = []string{"__Queue"} },
		"workflow class":    func(e *ExportSpec) { e.Classes = []string{"__Workflow.billing"} },
		"cron class":        func(e *ExportSpec) { e.Classes = []string{"Cart.cron"} },
		"comma class":       func(e *ExportSpec) { e.Classes = []string{"Cart,Order"} },
		"repeated class":    func(e *ExportSpec) { e.Classes = []string{"Cart", "Cart"} },
		"bare table":        func(e *ExportSpec) { e.ExcludeTables = []string{"audit"} },
		"record over queue": func(e *ExportSpec) { e.MaxRecordBytes, e.QueueBytes = 2048, 1024 },
		"unknown sink":      func(e *ExportSpec) { e.Sink = "BlobStream" },
		"kafka on bucket":   func(e *ExportSpec) { e.Kafka = kafkaExport().Kafka },
		"bucket name":       func(e *ExportSpec) { e.Bucket = &ExportBucketSpec{Name: "Acme_Export"} },
		"kafka missing":     func(e *ExportSpec) { e.Sink = "Kafka" },
		"kafka and bucket":  func(e *ExportSpec) { *e = *kafkaExport(); e.Bucket = &ExportBucketSpec{} },
		"broker port":       func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Brokers = []string{"kafka-0.kafka"} },
		"broker list":       func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Brokers = []string{"a:9092,b:9092"} },
		"topic":             func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Topic = "celld changes" },
		"no egress":         func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Egress = CollectorEgress{} },
		"host bits":         func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Egress.CIDR = "10.20.0.1/16" },
		"cidr namespace":    func(e *ExportSpec) { *e = *kafkaExport(); e.Kafka.Egress.Namespace = "kafka" },
	} {
		f := valid()
		f.Spec.Export = &ExportSpec{Sink: "Bucket"}
		mutate(f.Spec.Export)
		if f.Validate() == nil {
			t.Errorf("%s: invalid export accepted", name)
		}
	}
	f := valid()
	f.Spec.Previews = &FleetPreviewsSpec{}
	f.Spec.Previews.Storage.Bucket = "preview-bucket"
	f.Spec.Export = &ExportSpec{Sink: "Bucket", Bucket: &ExportBucketSpec{Name: "preview-bucket"}}
	if validateExport(&f.Spec) == nil {
		t.Error("export into the preview bucket accepted")
	}
}

func TestExportEnvIsReserved(t *testing.T) {
	value := "1"
	for _, name := range []string{"CELLD_EXPORT", "CELLD_EXPORT_SINK", "CELLD_EXPORT_BROKERS"} {
		if validateFleetEnv([]FleetEnvVar{{Name: name, Value: &value}}) == nil {
			t.Errorf("%s accepted through spec.env", name)
		}
	}
}

func TestKafkaBrokerPorts(t *testing.T) {
	got := KafkaBrokerPorts([]string{"a:9092", "b:9092", "[fd00::1]:9093"})
	if len(got) != 2 || got[0] != 9092 || got[1] != 9093 {
		t.Fatalf("ports = %v", got)
	}
}
