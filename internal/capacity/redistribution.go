package capacity

import (
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

// Maps are immutable snapshots replaced on each accepted observation. The journal
// retains both sides of an addition across process/leader restarts.
type Load struct {
	CPU, MemoryMiB int64
	Pressure       bool
}
type Addition struct {
	Outcome string
	Before  map[string]Load
	Target  int32
	Since   time.Time
	Samples int32
}

// Positive assessments require complete, fresh, advancing observations; reads
// between sample intervals can only invalidate a window. A gap resets the window;
// policy edits, pause and membership changes do not discard a hold.
func assessAddition(p fleet.CapacityPolicy, s *State, now time.Time, current int32, count bool) string {
	a := s.Addition
	if a == nil {
		return ""
	}
	if current < a.Target {
		return "ObservingRedistribution"
	}
	var beforeCPU, afterCPU int64
	hotBefore, hotAfter, newcomers, busyNew := 0, 0, 0, false
	hot := func(v Load) bool {
		return v.Pressure || v.CPU >= int64(p.CPUHighMillicores) || v.MemoryMiB >= int64(p.MemoryHighMiB)
	}
	for _, v := range a.Before {
		beforeCPU += v.CPU
		if hot(v) {
			hotBefore++
		}
	}
	for id, v := range s.Load {
		afterCPU += v.CPU
		if _, exists := a.Before[id]; exists {
			if hot(v) {
				hotAfter++
			}
		} else {
			newcomers++
			busyNew = busyNew || v.CPU >= int64(p.CPULowMillicores)
		}
	}
	// Changed/missing incumbent identities cannot be counted as improvement.
	for id := range a.Before {
		if _, exists := s.Load[id]; !exists {
			a.Since = time.Time{}
			a.Samples = 0
			return "RedistributionUnknown"
		}
	}
	if newcomers < int(a.Target)-len(a.Before) {
		return "RedistributionUnknown"
	}
	// Absolute floor avoids releasing on tiny CPU noise. Memory growth alone may
	// be an idle cache and is not evidence of independently increasing demand.
	demandGrew := afterCPU > beforeCPU+max(beforeCPU/10, int64(p.CPULowMillicores))
	outcome := "ineffective"
	if busyNew || hotAfter < hotBefore || demandGrew {
		outcome = "improved"
	}
	if outcome != a.Outcome || a.Since.IsZero() {
		a.Outcome = outcome
		a.Since = now
		a.Samples = 0
	}
	if !count {
		return "ObservingRedistribution"
	}
	a.Samples++
	if a.Samples < p.MinSamples || now.Sub(a.Since) < Seconds(p.RedistributionObservationSeconds) {
		if s.IneffectiveBatches >= 2 {
			return "LoadNotRedistributed"
		}
		return "ObservingRedistribution"
	}
	if outcome == "improved" {
		s.IneffectiveBatches = 0
		s.Addition = nil
		return ""
	}
	if s.IneffectiveBatches < 2 {
		s.IneffectiveBatches++
		if s.IneffectiveBatches < 2 {
			s.Addition = nil
			return ""
		}
	}
	return "LoadNotRedistributed"
}
