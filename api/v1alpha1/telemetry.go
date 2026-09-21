package v1alpha1

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

func validateTelemetry(s *TelemetrySpec) error {
	if s == nil {
		return nil
	}
	u, err := url.Parse(s.CollectorURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(s.CollectorURL, "?") || strings.HasSuffix(s.CollectorURL, "#") {
		return fmt.Errorf("telemetry.collectorURL must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if u.Port() != "" {
		p, err := strconv.Atoi(u.Port())
		if err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("telemetry.collectorURL has an invalid port")
		}
	}
	if s.Egress.CIDR != "" {
		p, err := netip.ParsePrefix(s.Egress.CIDR)
		if err != nil || !p.IsValid() || p.Bits() != p.Addr().BitLen() || p.Addr() != p.Masked().Addr() || len(s.Egress.PodLabels) != 0 || s.Egress.Namespace != "" {
			return fmt.Errorf("telemetry.egress.cidr must identify one collector IP")
		}
		if host, err := netip.ParseAddr(u.Hostname()); err == nil && host != p.Addr() {
			return fmt.Errorf("telemetry collector URL and CIDR address differ")
		}
	} else {
		if len(s.Egress.PodLabels) == 0 {
			return fmt.Errorf("telemetry.egress requires cidr or podLabels")
		}
		if s.Egress.Namespace != "" && len(validation.IsDNS1123Label(s.Egress.Namespace)) != 0 {
			return fmt.Errorf("telemetry.egress.namespace is invalid")
		}
		for k, v := range s.Egress.PodLabels {
			if len(validation.IsQualifiedName(k)) != 0 || len(validation.IsValidLabelValue(v)) != 0 {
				return fmt.Errorf("telemetry.egress.podLabels contains an invalid label")
			}
		}
	}
	switch s.Sampler {
	case "", "always_on", "always_off", "parentbased_always_on", "parentbased_always_off":
		if s.SamplerArg != "" {
			return fmt.Errorf("telemetry.samplerArg requires a ratio sampler")
		}
	case "traceidratio", "parentbased_traceidratio":
		ratio, err := strconv.ParseFloat(s.SamplerArg, 64)
		if err != nil || ratio < 0 || ratio > 1 || !regexp.MustCompile(`^(0(\.\d+)?|1(\.0+)?)$`).MatchString(s.SamplerArg) {
			return fmt.Errorf("telemetry.samplerArg must be between 0 and 1")
		}
	default:
		return fmt.Errorf("telemetry.sampler is unsupported")
	}
	if s.FlushMilliseconds < 0 || s.FlushBytes < 0 {
		return fmt.Errorf("telemetry flush settings must be positive")
	}
	if s.HeadersSecretKeyRef != nil && (len(validation.IsDNS1123Subdomain(s.HeadersSecretKeyRef.Name)) != 0 || len(validation.IsConfigMapKey(s.HeadersSecretKeyRef.Key)) != 0) {
		return fmt.Errorf("telemetry.headersSecretKeyRef is invalid")
	}
	return nil
}
