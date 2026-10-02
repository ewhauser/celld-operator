package v1alpha1

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ExportSpec maps to celld's change-export environment contract (the
// CELLD_EXPORT_* variables). Omitted fields keep celld's defaults.
// +kubebuilder:validation:XValidation:rule="self.sink == 'Kafka' ? has(self.kafka) && !has(self.bucket) : !has(self.kafka)",message="kafka is required with the Kafka sink and only valid with it; bucket is only valid with the Bucket sink"
// +kubebuilder:validation:XValidation:rule="!has(self.maxRecordBytes) || !has(self.queueBytes) || self.maxRecordBytes <= self.queueBytes",message="maxRecordBytes must not exceed queueBytes"
type ExportSpec struct {
	// Where records go: Bucket writes Parquet objects to the export bucket;
	// Kafka produces to a topic and needs a celld built with the export-kafka feature.
	// +kubebuilder:default=Bucket
	// +kubebuilder:validation:Enum=Bucket;Kafka
	Sink string `json:"sink,omitempty"`
	// Durable Object classes to export (CELLD_EXPORT_CLASSES). Unset exports
	// every application class plus D1 databases and KV namespaces. Queue
	// brokers, Workflow instances and cron cells are never exported.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[^,\s]+$`
	// +listType=set
	Classes []string `json:"classes,omitempty"`
	// Tables never exported, as Class.table (CELLD_EXPORT_TABLES).
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^,.\s]+\.[^,\s]+$`
	// +listType=set
	ExcludeTables []string `json:"excludeTables,omitempty"`
	// Capture memory above which a transaction is exported as bulk
	// (CELLD_EXPORT_MAX_TX_BYTES). celld default 4194304.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxTransactionBytes int64 `json:"maxTransactionBytes,omitempty"`
	// Fragment size (CELLD_EXPORT_MAX_RECORD_BYTES). celld default 1048576.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxRecordBytes int64 `json:"maxRecordBytes,omitempty"`
	// Shared memory budget for commits waiting on durability and records
	// waiting on the sink (CELLD_EXPORT_QUEUE_BYTES). celld default 268435456;
	// size the memory limit for it.
	// +optional
	// +kubebuilder:validation:Minimum=1
	QueueBytes int64 `json:"queueBytes,omitempty"`
	// Bucket sink settings.
	// +optional
	Bucket *ExportBucketSpec `json:"bucket,omitempty"`
	// Kafka sink settings, required with the Kafka sink.
	// +optional
	Kafka *ExportKafkaSpec `json:"kafka,omitempty"`
}

type ExportBucketSpec struct {
	// A bucket for the export other than the fleet bucket, on the same
	// endpoint and credentials (CELLD_EXPORT_BUCKET). The fleet's
	// ServiceAccount must be able to write it.
	// +optional
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
	Name string `json:"name,omitempty"`
	// Flush interval and watermark cadence (CELLD_EXPORT_FLUSH_MS). celld default 10000.
	// +optional
	// +kubebuilder:validation:Minimum=1
	FlushMilliseconds int64 `json:"flushMilliseconds,omitempty"`
	// Buffered bytes that trigger an early flush (CELLD_EXPORT_FLUSH_BYTES). celld default 8388608.
	// +optional
	// +kubebuilder:validation:Minimum=1
	FlushBytes int64 `json:"flushBytes,omitempty"`
	// Days after which a node deletes its export objects
	// (CELLD_EXPORT_RETENTION). Unset leaves the lifecycle to the bucket.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=36500
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

type ExportKafkaSpec struct {
	// Bootstrap servers as host:port (CELLD_EXPORT_KAFKA_BROKERS).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=261
	// +kubebuilder:validation:items:Pattern=`^[^,\s]+:[0-9]{1,5}$`
	// +listType=set
	Brokers []string `json:"brokers"`
	// The topic (CELLD_EXPORT_TOPIC). celld default celld-changes. Create it
	// first; the sink never creates it.
	// +optional
	// +kubebuilder:validation:MaxLength=249
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	Topic string `json:"topic,omitempty"`
	// How long the sink retries a record before it counts as dropped
	// (CELLD_EXPORT_RETRY_MS). celld default 30000.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RetryMilliseconds int64 `json:"retryMilliseconds,omitempty"`
	// Same-namespace Secret key holding librdkafka properties (TLS, SASL,
	// compression), one name=value per line, read as
	// CELLD_EXPORT_KAFKA_PROPERTIES. Every key of the Secret is mounted
	// read-only under /etc/celld/export, so the properties can name files the
	// same Secret carries, such as ssl.ca.location=/etc/celld/export/ca.crt.
	// +optional
	PropertiesSecretKeyRef *SecretKeyRef `json:"propertiesSecretKeyRef,omitempty"`
	// The brokers, for one TCP egress rule on the brokers' ports: labeled
	// broker Pods, or a cidr that may be a whole network, such as a managed
	// cluster's subnets.
	Egress CollectorEgress `json:"egress"`
}

// Never exported by celld, which refuses to start when the class list names one.
func unexportableClass(c string) bool {
	return c == "__Queue" || c == "__Workflow" || strings.HasPrefix(c, "__Workflow.") || strings.HasSuffix(c, ".cron")
}

var kafkaTopic = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,249}$`)

// KafkaBrokerPorts are the distinct broker ports, in first-seen order.
func KafkaBrokerPorts(brokers []string) []int32 {
	var ports []int32
	for _, b := range brokers {
		_, p, err := net.SplitHostPort(b)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		port := int32(n)
		seen := false
		for _, q := range ports {
			seen = seen || q == port
		}
		if !seen {
			ports = append(ports, port)
		}
	}
	return ports
}

func validateExport(s *CelldFleetSpec) error {
	e := s.Export
	if e == nil {
		return nil
	}
	if e.Sink != "" && e.Sink != "Bucket" && e.Sink != "Kafka" {
		return fmt.Errorf("export.sink must be Bucket or Kafka")
	}
	if len(e.Classes) > 64 || len(e.ExcludeTables) > 128 {
		return fmt.Errorf("export lists are too long")
	}
	seen := map[string]bool{}
	for _, c := range e.Classes {
		if c == "" || len(c) > 128 || strings.ContainsAny(c, ", \t\r\n") || seen[c] {
			return fmt.Errorf("export.classes entry %q is invalid or repeated", c)
		}
		if unexportableClass(c) {
			return fmt.Errorf("export.classes may not name %q: queue brokers, Workflow instances and cron cells are never exported", c)
		}
		seen[c] = true
	}
	seen = map[string]bool{}
	for _, t := range e.ExcludeTables {
		class, table, ok := strings.Cut(t, ".")
		if !ok || class == "" || table == "" || len(t) > 256 || strings.ContainsAny(t, ", \t\r\n") || seen[t] {
			return fmt.Errorf("export.excludeTables entry %q must be a distinct Class.table", t)
		}
		seen[t] = true
	}
	if e.MaxTransactionBytes < 0 || e.MaxRecordBytes < 0 || e.QueueBytes < 0 {
		return fmt.Errorf("export byte limits must be positive")
	}
	if e.MaxRecordBytes > 0 && e.QueueBytes > 0 && e.MaxRecordBytes > e.QueueBytes {
		return fmt.Errorf("export.maxRecordBytes must not exceed export.queueBytes")
	}
	if e.Sink == "Kafka" {
		if e.Kafka == nil || e.Bucket != nil {
			return fmt.Errorf("the Kafka export sink requires export.kafka and does not take export.bucket")
		}
		return validateExportKafka(e.Kafka)
	}
	if e.Kafka != nil {
		return fmt.Errorf("export.kafka requires the Kafka sink")
	}
	if b := e.Bucket; b != nil {
		if b.FlushMilliseconds < 0 || b.FlushBytes < 0 || b.RetentionDays < 0 || b.RetentionDays > 36500 {
			return fmt.Errorf("export.bucket flush and retention settings must be positive")
		}
		if b.Name != "" {
			if !bucketNamePattern.MatchString(b.Name) {
				return fmt.Errorf("export.bucket.name must be a canonical bucket name")
			}
			if s.Previews != nil && b.Name == s.Previews.Storage.Bucket {
				return fmt.Errorf("export.bucket.name must not be the preview bucket")
			}
		}
	}
	return nil
}

func validateExportKafka(k *ExportKafkaSpec) error {
	if len(k.Brokers) == 0 || len(k.Brokers) > 16 {
		return fmt.Errorf("export.kafka.brokers must list 1 to 16 bootstrap servers")
	}
	seen := map[string]bool{}
	for _, b := range k.Brokers {
		host, port, err := net.SplitHostPort(b)
		n, perr := strconv.Atoi(port)
		if err != nil || host == "" || perr != nil || n < 1 || n > 65535 || strings.ContainsAny(b, ", \t\r\n") || seen[b] {
			return fmt.Errorf("export.kafka.brokers entry %q must be a distinct host:port", b)
		}
		seen[b] = true
	}
	if k.Topic != "" && !kafkaTopic.MatchString(k.Topic) {
		return fmt.Errorf("export.kafka.topic is not a valid Kafka topic name")
	}
	if k.RetryMilliseconds < 0 {
		return fmt.Errorf("export.kafka.retryMilliseconds must be positive")
	}
	if r := k.PropertiesSecretKeyRef; r != nil && (len(validation.IsDNS1123Subdomain(r.Name)) != 0 || len(validation.IsConfigMapKey(r.Key)) != 0) {
		return fmt.Errorf("export.kafka.propertiesSecretKeyRef is invalid")
	}
	g := k.Egress
	if (g.CIDR != "") == (len(g.PodLabels) != 0) {
		return fmt.Errorf("export.kafka.egress requires exactly one of cidr or podLabels")
	}
	if g.CIDR != "" {
		p, err := netip.ParsePrefix(g.CIDR)
		if err != nil || p.Addr() != p.Masked().Addr() || g.Namespace != "" {
			return fmt.Errorf("export.kafka.egress.cidr must be a network address in CIDR form")
		}
		return nil
	}
	if g.Namespace != "" && len(validation.IsDNS1123Label(g.Namespace)) != 0 {
		return fmt.Errorf("export.kafka.egress.namespace is invalid")
	}
	for key, v := range g.PodLabels {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(v)) != 0 {
			return fmt.Errorf("export.kafka.egress.podLabels contains an invalid label")
		}
	}
	return nil
}
