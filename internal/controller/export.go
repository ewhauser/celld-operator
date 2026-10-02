package controller

import (
	"strconv"
	"strings"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const (
	exportKafkaVolume = "export-kafka"
	exportKafkaDir    = "/etc/celld/export"
)

func exportKafka(e *fleet.ExportSpec) *fleet.ExportKafkaSpec {
	if e == nil || e.Sink != "Kafka" {
		return nil
	}
	return e.Kafka
}

// exportEnv renders spec.export as celld's CELLD_EXPORT_* contract. Omitted
// fields are left unset so celld's own defaults apply.
func exportEnv(e *fleet.ExportSpec) []corev1.EnvVar {
	if e == nil {
		return nil
	}
	env := []corev1.EnvVar{{Name: "CELLD_EXPORT", Value: "1"}}
	add := func(name, value string) { env = append(env, corev1.EnvVar{Name: name, Value: value}) }
	positive := func(name string, value int64) {
		if value > 0 {
			add(name, strconv.FormatInt(value, 10))
		}
	}
	if len(e.Classes) > 0 {
		add("CELLD_EXPORT_CLASSES", strings.Join(e.Classes, ","))
	}
	if len(e.ExcludeTables) > 0 {
		add("CELLD_EXPORT_TABLES", strings.Join(e.ExcludeTables, ","))
	}
	positive("CELLD_EXPORT_MAX_TX_BYTES", e.MaxTransactionBytes)
	positive("CELLD_EXPORT_MAX_RECORD_BYTES", e.MaxRecordBytes)
	positive("CELLD_EXPORT_QUEUE_BYTES", e.QueueBytes)
	if k := exportKafka(e); k != nil {
		add("CELLD_EXPORT_SINK", "kafka")
		add("CELLD_EXPORT_KAFKA_BROKERS", strings.Join(k.Brokers, ","))
		if k.Topic != "" {
			add("CELLD_EXPORT_TOPIC", k.Topic)
		}
		positive("CELLD_EXPORT_RETRY_MS", k.RetryMilliseconds)
		if k.PropertiesSecretKeyRef != nil {
			add("CELLD_EXPORT_KAFKA_PROPERTIES", exportKafkaDir+"/"+k.PropertiesSecretKeyRef.Key)
		}
		return env
	}
	add("CELLD_EXPORT_SINK", "bucket")
	if b := e.Bucket; b != nil {
		if b.Name != "" {
			add("CELLD_EXPORT_BUCKET", b.Name)
		}
		positive("CELLD_EXPORT_FLUSH_MS", b.FlushMilliseconds)
		positive("CELLD_EXPORT_FLUSH_BYTES", b.FlushBytes)
		if b.RetentionDays > 0 {
			add("CELLD_EXPORT_RETENTION", strconv.Itoa(int(b.RetentionDays))+"d")
		}
	}
	return env
}
