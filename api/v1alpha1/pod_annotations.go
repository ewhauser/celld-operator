package v1alpha1

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Keep these guards synchronized with the CRD rule. The operator writes these
// keys on member Pods itself: the restart token and mesh membership.
const operatorAnnotationPrefix = "celld.eric.dev/"

var ownedPodAnnotations = map[string]bool{
	"sidecar.istio.io/nativeSidecar":                   true,
	"traffic.sidecar.istio.io/excludeOutboundIPRanges": true,
}

const maxPodAnnotationValue = 4096

func validatePodAnnotations(annotations map[string]string) error {
	if len(annotations) > 32 {
		return fmt.Errorf("podAnnotations may contain at most 32 entries")
	}
	for k, v := range annotations {
		if len(validation.IsQualifiedName(k)) != 0 {
			return fmt.Errorf("podAnnotations key %q is not a qualified name", k)
		}
		if strings.HasPrefix(k, operatorAnnotationPrefix) || ownedPodAnnotations[k] {
			return fmt.Errorf("podAnnotations key %q is operator-owned", k)
		}
		if len(v) > maxPodAnnotationValue {
			return fmt.Errorf("podAnnotations %q value exceeds %d bytes", k, maxPodAnnotationValue)
		}
	}
	return nil
}
