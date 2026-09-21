package controller

import (
	"errors"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
)

func ValidateSurvivors(p fleet.CapacityPolicy, o capacity.Observation, identities []string, donor string, now time.Time) error {
	if !capacity.LowDemand(p, o, int32(len(identities))) || o.At.After(now) || now.Sub(o.At) > capacity.Seconds(p.MaxAgeSeconds) {
		return errors.New("survivor health or capacity evidence incomplete")
	}
	if len(identities) < 2 || len(o.Samples) != len(identities) {
		return errors.New("no survivor capacity")
	}
	seen := map[string]bool{}
	var removed *capacity.Sample
	for i := range o.Samples {
		s := &o.Samples[i]
		if !slices.Contains(identities, s.Identity) || seen[s.Identity] {
			return errors.New("container generation changed")
		}
		seen[s.Identity] = true
		if s.Identity == donor {
			removed = s
		}
	}
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
	return nil
}
