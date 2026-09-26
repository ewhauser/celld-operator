// Package capacity evaluates observations without IO or wall-clock access.
package capacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

// Sample binds both source clocks and operator receipt clocks to a Pod/container incarnation.
// Missing/invalid source values are represented by zero timestamps, never zero usage.
type Sample struct {
	Identity                                               string
	Ready, Pressured, Backlog                              bool
	RuntimeMemoryMiB                                       int64
	CPU, MemoryMiB                                         int64
	RuntimeAt, RuntimeReceived, MetricsAt, MetricsReceived time.Time
	Window                                                 time.Duration
}
type Observation struct {
	At       time.Time
	Complete bool
	Samples  []Sample
}
type Stamp struct{ Runtime, Metrics time.Time }

// State is retained with bounded current-operation state. Status is never an input.
type State struct {
	Load               map[string]Load
	Addition           *Addition
	IneffectiveBatches int32

	// Actionable is refreshed by each evaluation; cached recommendations are informational.
	Actionable                                                     bool
	Config, Membership                                             string
	LastObservation, HighSince, LowSince, PendingSince, LastAction time.Time
	HighSamples, LowSamples                                        int32
	Stamps                                                         map[string]Stamp
	ManualTarget                                                   int32
	LastManual                                                     int32
	Decision                                                       fleet.CapacityStatus
}

func Seconds(n int32) time.Duration { return time.Duration(n) * time.Second }
func fresh(t, now time.Time, age time.Duration) bool {
	return !t.IsZero() && !t.After(now) && now.Sub(t) <= age
}
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func reset(s *State) {
	s.HighSince = time.Time{}
	s.LowSince = time.Time{}
	s.HighSamples = 0
	s.LowSamples = 0
	if s.Addition != nil {
		s.Addition.Since = time.Time{}
		s.Addition.Samples = 0
	}
}

// Evaluate requires 100% coverage for either direction. Pending replicas reserve
// the entire addition budget and never dilute per-node pressure or count as useful.
func Evaluate(p fleet.CapacityPolicy, old State, o Observation, current int32) State {
	s := old
	s.Actionable = false
	s.Stamps = make(map[string]Stamp, len(old.Stamps))
	if old.Addition != nil {
		addition := *old.Addition
		s.Addition = &addition
	}

	s.Decision = fleet.CapacityStatus{Mode: p.Mode, DesiredReplicas: current}
	hold := func(reason, message string) State { s.Decision.Reason = reason; s.Decision.Message = message; return s }
	if p.Validate() != nil || current < 1 || current > 100 || o.At.IsZero() || o.At.Before(old.LastObservation) || o.At.Before(old.LastAction) || o.At.Before(old.PendingSince) {
		reset(&s)
		return hold("InvalidObservation", "Invalid configuration, replica count or clock rewind; history retained")
	}
	config := fingerprint(p)
	identities := make([]string, 0, len(o.Samples))
	for _, v := range o.Samples {
		identities = append(identities, v.Identity)
	}
	slices.Sort(identities)
	membership := fingerprint(identities)
	if config != s.Config || membership != s.Membership {
		reset(&s)
		s.Config = config
		s.Membership = membership
	}
	intermediate := !old.LastObservation.IsZero() && o.At.Sub(old.LastObservation) < Seconds(p.SampleIntervalSeconds)
	if !fresh(old.LastObservation, o.At, Seconds(p.MaxAgeSeconds)) {
		reset(&s)
	}
	if !intermediate {
		s.LastObservation = o.At
	}
	complete := o.Complete && len(o.Samples) == int(current)
	high, low, newSources := false, true, true
	seen := map[string]bool{}
	for _, v := range o.Samples {
		validID := v.Identity != "" && !seen[v.Identity]
		seen[v.Identity] = true
		runtimeFresh := validID && fresh(v.RuntimeAt, o.At, Seconds(p.MaxAgeSeconds)) && fresh(v.RuntimeReceived, o.At, Seconds(p.MaxAgeSeconds)) && !v.RuntimeAt.After(v.RuntimeReceived)
		if runtimeFresh && v.Ready {
			s.Decision.UsefulReplicas++
		}
		metricsFresh := fresh(v.MetricsAt, o.At, Seconds(p.MaxAgeSeconds)) && fresh(v.MetricsReceived, o.At, Seconds(p.MaxAgeSeconds)) && !v.MetricsAt.After(v.MetricsReceived) && v.Window >= Seconds(p.MinWindowSeconds) && v.Window <= Seconds(p.MaxWindowSeconds) && v.CPU >= 0 && v.MemoryMiB >= 0
		if !runtimeFresh || !metricsFresh {
			complete = false
			low = false
			continue
		}
		s.Decision.CoveredReplicas++
		prev := old.Stamps[v.Identity]
		if !v.RuntimeAt.After(prev.Runtime) || !v.MetricsAt.After(prev.Metrics) {
			newSources = false
		}
		s.Stamps[v.Identity] = Stamp{v.RuntimeAt, v.MetricsAt}
		pressured := v.Pressured || v.Backlog || v.CPU >= int64(p.CPUHighMillicores) || v.MemoryMiB >= int64(p.MemoryHighMiB)
		high = high || pressured
		low = low && !pressured && v.CPU < int64(p.CPULowMillicores) && v.MemoryMiB < int64(p.MemoryLowMiB)
	}
	// Intermediate reads can invalidate a window, but must not consume an
	// observation slot or advance source watermarks used for positive evidence.
	if intermediate {
		s.Stamps = old.Stamps
	}
	s.Decision.PendingReplicas = max(0, current-s.Decision.UsefulReplicas)
	if s.Decision.PendingReplicas > 0 {
		reset(&s)
		if s.PendingSince.IsZero() {
			s.PendingSince = o.At
		}
		if o.At.Sub(s.PendingSince) >= Seconds(p.ProvisioningTimeoutSeconds) {
			return hold("IneffectiveCapacity", "Capacity has not become useful before the provisioning deadline; check pressured incumbents, join readiness, scheduling, AZs and PVCs; no further addition")
		}
		return hold("PendingCapacity", "Unready or unobserved replicas reserve capacity; no additional scaling while provisioning")
	}
	s.PendingSince = time.Time{}
	if !complete {
		reset(&s)
		return hold("IncompleteMetrics", "Require fresh runtime and CPU/memory samples from every expected replica")
	}
	if current < p.MinReplicas {
		high = true
		low = false
	}
	if intermediate {
		s.Load = loads(o)
		_ = assessAddition(p, &s, o.At, current, false)
		if config != old.Config || membership != old.Membership {
			return hold("PolicyChanged", "Policy or membership changed; a new stabilization window is required")
		}
		if (!high && !s.HighSince.IsZero()) || (!low && !s.LowSince.IsZero()) {
			reset(&s)
			return hold("WindowInterrupted", "Observed demand changed between sample intervals; stabilization restarted")
		}
		s.Decision = old.Decision
		return s
	}
	if !newSources {
		reset(&s)
		return hold("RepeatedSamples", "Source timestamps did not advance for every replica; stabilization restarted")
	}
	s.Load = loads(o)
	if reason := assessAddition(p, &s, o.At, current, true); reason != "" && high && current >= p.MinReplicas {
		s.HighSince = time.Time{}
		s.HighSamples = 0
		return hold(reason, "Observe added capacity; repeated ready-but-idle additions are held until load redistributes or independent CPU demand grows")
	}
	if high {
		s.LowSince = time.Time{}
		s.LowSamples = 0
		if s.HighSince.IsZero() {
			s.HighSince = o.At
		}
		s.HighSamples++
		if s.HighSamples < p.MinSamples || o.At.Sub(s.HighSince) < Seconds(p.ScaleOutStabilizationSeconds) {
			return hold("StabilizingOut", "Sustained pressure window is incomplete")
		}
		if !s.LastAction.IsZero() && o.At.Sub(s.LastAction) < Seconds(p.ScaleOutCooldownSeconds) {
			return hold("RateLimited", "Scale-out cooldown since the last durable action")
		}
		if current >= p.MaxReplicas {
			return hold("AtMaximum", "Replica upper bound reached; pressure remains")
		}
		s.Actionable = true
		s.Decision.DesiredReplicas = min(p.MaxReplicas, current+p.ScaleOutStep)
		return hold("ScaleOutRecommended", "Sustained per-node pressure; bounded addition requires lifecycle authorization")
	}
	s.HighSince = time.Time{}
	s.HighSamples = 0
	if low {
		if s.LowSince.IsZero() {
			s.LowSince = o.At
		}
		s.LowSamples++
		if s.LowSamples < p.MinSamples || o.At.Sub(s.LowSince) < Seconds(p.ScaleInStabilizationSeconds) {
			return hold("StabilizingIn", "Full low-demand window is incomplete")
		}
		if !s.LastAction.IsZero() && o.At.Sub(s.LastAction) < Seconds(p.ScaleInCooldownSeconds) {
			return hold("RateLimited", "Scale-in cooldown since the last durable action")
		}
		if current <= p.MinReplicas {
			return hold("AtMinimum", "Replica lower bound reached")
		}
		s.Actionable = true
		s.Decision.DesiredReplicas = current - 1
		return hold("ScaleInRecommended", "Low demand across every node; one removal still requires qualified lifecycle evidence")
	}
	s.HighSince, s.LowSince = time.Time{}, time.Time{}
	s.HighSamples, s.LowSamples = 0, 0
	return hold("WithinThresholds", fmt.Sprintf("All %d replicas observed; demand lies between thresholds", current))
}

// RecordAction is called once per applied replica change, by the reconcile that
// writes it, never on a shadow recommendation. An addition is judged for
// redistribution only when Load is the observation of the from members.
func RecordAction(s *State, now time.Time, from, to int32) {
	s.Actionable = false
	s.LastAction = now
	// Retain the qualified low window while a removal intent awaits issuance.
	// Any subsequent pressure, missing sample or gap still resets it in Evaluate.
	if to > from {
		if len(s.Load) == int(from) {
			s.Addition = &Addition{Before: s.Load, Target: to}
		}
		reset(s)
		s.PendingSince = now
	} else {
		s.HighSince = time.Time{}
		s.HighSamples = 0
	}
}

// LowDemand revalidates the current complete observation immediately before an
// already-recorded contraction. It cannot authorize a new operation or replace
// lifecycle recovery evidence. Unknown/repeated clocks remain conservative.
func LowDemand(p fleet.CapacityPolicy, o Observation, current int32) bool {
	if p.Validate() != nil || !o.Complete || o.At.IsZero() || len(o.Samples) != int(current) {
		return false
	}
	seen := map[string]bool{}
	for _, v := range o.Samples {
		if v.Identity == "" || seen[v.Identity] || !v.Ready || v.Pressured || v.Backlog || v.CPU < 0 || v.MemoryMiB < 0 || v.CPU >= int64(p.CPULowMillicores) || v.MemoryMiB >= int64(p.MemoryLowMiB) || v.Window < Seconds(p.MinWindowSeconds) || v.Window > Seconds(p.MaxWindowSeconds) {
			return false
		}
		seen[v.Identity] = true
		for _, stamp := range []time.Time{v.RuntimeAt, v.RuntimeReceived, v.MetricsAt, v.MetricsReceived} {
			if !fresh(stamp, o.At, Seconds(p.MaxAgeSeconds)) {
				return false
			}
		}
		if v.RuntimeAt.After(v.RuntimeReceived) || v.MetricsAt.After(v.MetricsReceived) {
			return false
		}
	}
	return true
}

func loads(o Observation) map[string]Load {
	result := make(map[string]Load, len(o.Samples))
	for _, v := range o.Samples {
		result[v.Identity] = Load{CPU: v.CPU, MemoryMiB: v.MemoryMiB, Pressure: v.Pressured || v.Backlog}
	}
	return result
}
