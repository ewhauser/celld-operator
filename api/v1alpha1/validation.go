package v1alpha1

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (f *CelldFleet) Default() {
	if f.Spec.BucketWorkload == "" {
		f.Spec.BucketWorkload = "Deployment"
	}
	if f.Spec.Capacity != nil {
		f.Spec.Capacity.Default()
	}
	if f.Spec.Replicas == 0 {
		f.Spec.Replicas = 3
	}
	if f.Spec.Storage.SizeGiB == 0 {
		f.Spec.Storage.SizeGiB = 10
	}
	if f.Spec.Placement.Mode == "" {
		f.Spec.Placement.Mode = "Strict"
	}
}

func (f *CelldFleet) Validate() error {
	s := f.Spec
	if s.RuntimeImage != "" && !ValidRuntimeImage(s.RuntimeImage) {
		return fmt.Errorf("runtimeImage must be an immutable, registry-qualified OCI sha256 digest")
	}
	if s.Maintenance != nil && len(s.Maintenance.RestartToken) > 128 {
		return fmt.Errorf("restartToken exceeds 128 characters")
	}
	if s.Capacity != nil {
		if err := s.Capacity.Validate(); err != nil {
			return err
		}
		if s.Capacity.MinReplicas < s.Placement.AZCount {
			return fmt.Errorf("capacity minimum must cover requested AZs")
		}
	}
	if err := validateTuning(&s); err != nil {
		return err
	}
	if err := validateFleetEnv(s.Env); err != nil {
		return err
	}
	switch {
	case s.BucketWorkload != "" && s.BucketWorkload != "Deployment" && s.BucketWorkload != "Ordered":
		return fmt.Errorf("bucketWorkload must be Deployment or Ordered")
	case s.BucketWorkload == "Ordered" && s.Profile != "Bucket":
		return fmt.Errorf("ordered bucketWorkload requires Bucket profile")
	case len(f.Name) > 40 || len(validation.IsDNS1123Label(f.Name)) != 0:
		return fmt.Errorf("fleet name must be a DNS label of at most 40 characters")
	case s.Qualification != "Experimental":
		return fmt.Errorf("qualification must be Experimental; production qualification is incomplete")
	case s.Profile != "Bucket" && s.Profile != "PersistentFleet":
		return fmt.Errorf("profile must be Bucket or PersistentFleet")
	case s.Replicas < 1 || s.Replicas > 100:
		return fmt.Errorf("replicas must be between 1 and 100")
	case s.ServiceAccountName == "" || len(validation.IsDNS1123Subdomain(s.ServiceAccountName)) != 0:
		return fmt.Errorf("serviceAccountName must reference an existing ServiceAccount")
	case !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`).MatchString(s.Storage.Bucket):
		return fmt.Errorf("storage.bucket must be a canonical bucket name (3-63 lowercase letters, digits or hyphens)")
	case !regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`).MatchString(s.Storage.Region):
		return fmt.Errorf("storage.region must be an explicit AWS region")
	case s.Storage.SizeGiB < 1 || s.Storage.SizeGiB > 16384:
		return fmt.Errorf("storage.sizeGiB must be between 1 and 16384")
	case s.Profile == "PersistentFleet" && (s.Storage.StorageClassName == "" || len(validation.IsDNS1123Subdomain(s.Storage.StorageClassName)) != 0):
		return fmt.Errorf("PersistentFleet requires storage.storageClassName")
	case s.Profile == "Bucket" && s.Storage.StorageClassName != "":
		return fmt.Errorf("bucket profile must not set storage.storageClassName")
	case s.Placement.AZCount < 1 || s.Placement.AZCount > 6 || int(s.Placement.AZCount) != len(s.Placement.Zones):
		return fmt.Errorf("placement.azCount must equal the explicit zones count (1-6)")
	case s.Replicas < s.Placement.AZCount:
		return fmt.Errorf("replicas must be at least placement.azCount")
	case s.Placement.Mode != "Strict" && s.Placement.Mode != "Relaxed":
		return fmt.Errorf("placement.mode must be Strict or Relaxed")
	}
	for i, z := range s.Placement.Zones {
		if !strings.HasPrefix(z, s.Storage.Region) || len(z) != len(s.Storage.Region)+1 || z[len(z)-1] < 'a' || z[len(z)-1] > 'z' || slices.Contains(s.Placement.Zones[:i], z) {
			return fmt.Errorf("zones must be unique standard AZ names in storage.region")
		}
	}
	return nil
}

// validateTuning mirrors the CRD rules for execution and lifecycle so that an
// admission bypass still fails closed before any template is generated.
func validateTuning(s *CelldFleetSpec) error {
	quantity := func(field, value string) (resource.Quantity, error) {
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return q, fmt.Errorf("%s must be a Kubernetes quantity: %w", field, err)
		}
		if q.Sign() <= 0 {
			return q, fmt.Errorf("%s must be positive", field)
		}
		return q, nil
	}
	e := s.EffectiveExecution()
	cpuRequest, err := quantity("execution.cpuRequest", e.CPURequest)
	if err != nil {
		return err
	}
	if e.CPULimit != "" {
		cpuLimit, err := quantity("execution.cpuLimit", e.CPULimit)
		if err != nil {
			return err
		}
		if cpuLimit.Cmp(cpuRequest) < 0 {
			return fmt.Errorf("execution.cpuLimit must be at least cpuRequest")
		}
	}
	memoryRequest, err := quantity("execution.memoryRequest", e.MemoryRequest)
	if err != nil {
		return err
	}
	memoryLimit, err := quantity("execution.memoryLimit", e.MemoryLimit)
	if err != nil {
		return err
	}
	if memoryLimit.Cmp(memoryRequest) < 0 {
		return fmt.Errorf("execution.memoryLimit must be at least memoryRequest")
	}
	if e.MaxResidentCells < 0 || e.MaxResidentCells > 1000000 {
		return fmt.Errorf("execution.maxResidentCells must be between 0 (unset) and 1000000")
	}
	if e.IdleEvictSeconds < 0 || e.IdleEvictSeconds > 86400 {
		return fmt.Errorf("execution.idleEvictSeconds must be between 0 (unset) and 86400")
	}
	l := s.EffectiveLifecycle()
	if l.ShutdownSeconds < 1 || l.ShutdownSeconds > 3600 {
		return fmt.Errorf("lifecycle.shutdownSeconds must be between 1 and 3600")
	}
	if l.TerminationGraceSeconds < l.ShutdownSeconds+terminationGraceHeadroom || l.TerminationGraceSeconds > 3605 {
		return fmt.Errorf("lifecycle.terminationGraceSeconds must exceed shutdownSeconds by at least %d", terminationGraceHeadroom)
	}
	return nil
}
