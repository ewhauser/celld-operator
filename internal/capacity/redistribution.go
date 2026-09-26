package capacity

import (
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

// Maps are immutable snapshots replaced on each accepted observation. The current operation
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
	// ObservedSince and ObservedSamples cover the continuous complete
	// observation of the addition, whatever its outcome; Since and Samples
	// cover the current outcome.
	ObservedSince   time.Time
	ObservedSamples int32
}

// Positive assessments require complete, fresh, advancing observations; reads
// between sample intervals can only invalidate a window. An addition is judged
// once one outcome holds for a whole window. Evidence that has not settled
// after twice the observation a judgment needs is mixed and counts as
// ineffective, never as relief. Whatever resets stable windows restarts the
// window and that bound; policy edits and pause do not discard a hold.
//
// An addition is judged against the incumbents it grew from, by container
// identity. An incumbent that restarts or is replaced never returns under its
// identity, and a contraction below the addition's target removes members the
// judgment needs, so no later observation can judge the addition. It is dropped
// as neither relief nor an ineffective batch: a changed incumbent is never
// evidence of improvement, and IneffectiveBatches is kept. The next addition
// still waits for a new stable window, the cooldown and useful capacity.
func assessAddition(p fleet.CapacityPolicy, s *State, now time.Time, current int32, count bool) string {
	a := s.Addition
	if a == nil {
		return ""
	}
	if current < a.Target || !observed(a.Before, s.Load) {
		s.Addition = nil
		return ""
	}
	var beforeCPU, afterCPU int64
	hotBefore, hotAfter, busyNew := 0, 0, false
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
			busyNew = busyNew || v.CPU >= int64(p.CPULowMillicores)
		}
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
	if a.ObservedSince.IsZero() {
		a.ObservedSince = now
	}
	if !count {
		return "ObservingRedistribution"
	}
	a.Samples++
	a.ObservedSamples++
	window := Seconds(p.RedistributionObservationSeconds)
	settled := a.Samples >= p.MinSamples && now.Sub(a.Since) >= window
	mixed := a.ObservedSamples >= 2*p.MinSamples && now.Sub(a.ObservedSince) >= 2*window
	if !settled && !mixed {
		if s.IneffectiveBatches >= 2 {
			return "LoadNotRedistributed"
		}
		return "ObservingRedistribution"
	}
	if settled && outcome == "improved" {
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

// observed reports whether every incumbent is present under its identity.
func observed(before, load map[string]Load) bool {
	for id := range before {
		if _, exists := load[id]; !exists {
			return false
		}
	}
	return true
}
