package v1alpha1

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

func (f *CelldFleet) Default() {
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
	switch {
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
