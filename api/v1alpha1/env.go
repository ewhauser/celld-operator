package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

var celldEnvName = regexp.MustCompile(`^CELLD_[A-Z][A-Z0-9_]*$`)

// Keep this guard synchronized with the CRD rule. These variables establish
// identity, addresses, storage, durability, process supervision, or policy.
var ownedCelldEnv = map[string]bool{
	"CELLD_NODE": true, "CELLD_ADVERTISE": true, "CELLD_BUCKET": true,
	"CELLD_DURABILITY": true, "CELLD_ADDR": true, "CELLD_INTERNAL_ADDR": true,
	"CELLD_WATCH": true, "CELLD_TTL_MS": true, "CELLD_SHUTDOWN_TOTAL_MS": true,
	"CELLD_TOKIO_THREADS": true, "CELLD_MAX_RESIDENT_CELLS": true,
	"CELLD_IDLE_EVICT_S": true,
}

func validateFleetEnv(entries []FleetEnvVar) error {
	if len(entries) > 32 {
		return fmt.Errorf("env may contain at most 32 entries")
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if len(e.Name) > 128 || !celldEnvName.MatchString(e.Name) || ownedCelldEnv[e.Name] || strings.HasPrefix(e.Name, "CELLD_REEXEC_") || strings.HasPrefix(e.Name, "CELLD_OTEL") || strings.HasPrefix(e.Name, "CELLD_UNSAFE_") || strings.HasPrefix(e.Name, "CELLD_TEST_") || strings.HasPrefix(e.Name, "CELLD_STRICT_") {
			return fmt.Errorf("env name %q is invalid or reserved", e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("duplicate env name %q", e.Name)
		}
		seen[e.Name] = true
		if (e.Value == nil) == (e.SecretKeyRef == nil) {
			return fmt.Errorf("env %q requires exactly one value source", e.Name)
		}
		if e.Value != nil && len(*e.Value) > 4096 {
			return fmt.Errorf("env %q literal exceeds 4096 bytes", e.Name)
		}
		if e.SecretKeyRef != nil && (len(validation.IsDNS1123Subdomain(e.SecretKeyRef.Name)) != 0 || len(validation.IsConfigMapKey(e.SecretKeyRef.Key)) != 0) {
			return fmt.Errorf("env %q has an invalid secretKeyRef", e.Name)
		}
	}
	return nil
}
