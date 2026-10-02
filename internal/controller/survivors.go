package controller

import (
	"errors"
	"fmt"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
)

func ValidateSurvivors(p fleet.CapacityPolicy, o capacity.Observation, identities []string, donor string, now time.Time) error {
	return validateDonors(p, o, identities, []string{donor}, now)
}

// validateDonors checks membership and freshness once for the observation,
// then projects each possible victim onto every survivor. Deployment victim
// selection is Kubernetes' choice, so every donor must still pass.
func validateDonors(p fleet.CapacityPolicy, o capacity.Observation, identities, donors []string, now time.Time) error {
	if !capacity.LowDemand(p, o, int32(len(identities))) || o.At.After(now) || now.Sub(o.At) > capacity.Seconds(p.MaxAgeSeconds) {
		return errors.New("survivor health or capacity evidence incomplete")
	}
	if len(identities) < 2 || len(o.Samples) != len(identities) {
		return errors.New("no survivor capacity")
	}
	samples := make(map[string]*capacity.Sample, len(identities))
	for _, id := range identities {
		samples[id] = nil
	}
	for i := range o.Samples {
		s := &o.Samples[i]
		if prior, expected := samples[s.Identity]; !expected || prior != nil {
			return errors.New("container generation changed")
		}
		samples[s.Identity] = s
	}
	for _, donor := range donors {
		removed := samples[donor]
		if removed == nil {
			return errors.New("donor sample missing")
		}
		for _, s := range o.Samples {
			if s.Identity == donor {
				continue
			}
			if removed.CPU >= int64(p.CPUHighMillicores)-s.CPU || max(removed.MemoryMiB, removed.RuntimeMemoryMiB) >= int64(p.MemoryHighMiB)-max(s.MemoryMiB, s.RuntimeMemoryMiB) {
				return fmt.Errorf("projected capacity exceeded on %s", s.Identity)
			}
		}
	}
	return nil
}
