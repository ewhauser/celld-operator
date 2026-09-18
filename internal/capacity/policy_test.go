package capacity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

func policy() fleet.CapacityPolicy { p := fleet.CapacityPolicy{}; p.Default(); return p }
func observation(at time.Time, n int32, cpu int64) Observation {
	o := Observation{At: at, Complete: true}
	for i := range n {
		o.Samples = append(o.Samples, Sample{Identity: fmt.Sprint(i), Ready: true, CPU: cpu, MemoryMiB: 100, RuntimeAt: at, RuntimeReceived: at, MetricsAt: at, MetricsReceived: at, Window: 15 * time.Second})
	}
	return o
}
func TestBurstBoundsAndRestart(t *testing.T) {
	p := policy()
	p.ScaleOutStep = 3
	p.MaxReplicas = 4
	now := time.Unix(10000, 0)
	s := State{}
	for i := range 3 {
		s = Evaluate(p, s, observation(now.Add(time.Duration(i)*15*time.Second), 3, 400), 3)
	}
	if s.Decision.DesiredReplicas != 4 {
		t.Fatal(s)
	}
	RecordAction(&s, now.Add(30*time.Second), 3, 4)
	b, _ := json.Marshal(s)
	var restarted State
	if err := json.Unmarshal(b, &restarted); err != nil {
		t.Fatal(err)
	}
	for i := 3; i < 25; i++ {
		o := observation(now.Add(time.Duration(i)*15*time.Second), 4, 400)
		o.Samples[3].Ready = false
		restarted = Evaluate(p, restarted, o, 4)
		if restarted.Decision.DesiredReplicas != 4 || restarted.Decision.UsefulReplicas != 3 || restarted.Decision.PendingReplicas != 1 {
			t.Fatal(restarted)
		}
	}
	o := observation(now.Add(645*time.Second), 4, 400)
	o.Samples[3].Ready = false
	restarted = Evaluate(p, restarted, o, 4)
	if restarted.Decision.Reason != "IneffectiveCapacity" {
		t.Fatal(restarted)
	}
}
func TestDecliningDemandFullWindowAndCooldown(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	s := State{LastAction: now}
	for i := range 60 {
		s = Evaluate(p, s, observation(now.Add(time.Duration(i)*15*time.Second), 4, 20), 4)
		if s.Decision.DesiredReplicas != 4 {
			t.Fatalf("early removal at %d: %+v", i, s)
		}
	}
	s = Evaluate(p, s, observation(now.Add(900*time.Second), 4, 20), 4)
	if s.Decision.DesiredReplicas != 3 {
		t.Fatal(s)
	}
	RecordAction(&s, now.Add(900*time.Second), 4, 3)
	s = Evaluate(p, s, observation(now.Add(915*time.Second), 3, 20), 3)
	if s.Decision.DesiredReplicas != 3 {
		t.Fatal(s)
	}
}
func TestUncertaintyResetsWindow(t *testing.T) {
	tests := map[string]func(*Observation){
		"partial":            func(o *Observation) { o.Complete = false },
		"missing pod":        func(o *Observation) { o.Samples = o.Samples[:2] },
		"stale metrics":      func(o *Observation) { o.Samples[0].MetricsAt = o.At.Add(-time.Minute) },
		"future runtime":     func(o *Observation) { o.Samples[0].RuntimeAt = o.At.Add(time.Second) },
		"stale receipt":      func(o *Observation) { o.Samples[0].MetricsReceived = o.At.Add(-time.Minute) },
		"missing metrics":    func(o *Observation) { o.Samples[0].MetricsAt = time.Time{} },
		"unknown runtime":    func(o *Observation) { o.Samples[0].RuntimeAt = time.Time{} },
		"duplicate identity": func(o *Observation) { o.Samples[1].Identity = o.Samples[0].Identity },
		"long window":        func(o *Observation) { o.Samples[0].Window = time.Hour },
		"short window":       func(o *Observation) { o.Samples[0].Window = time.Second },
		"negative":           func(o *Observation) { o.Samples[0].CPU = -1 },
		"unready":            func(o *Observation) { o.Samples[0].Ready = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := policy()
			now := time.Unix(10000, 0)
			s := State{}
			for i := range 2 {
				s = Evaluate(p, s, observation(now.Add(time.Duration(i)*15*time.Second), 3, 500), 3)
			}
			bad := observation(now.Add(30*time.Second), 3, 500)
			mutate(&bad)
			s = Evaluate(p, s, bad, 3)
			if s.Decision.DesiredReplicas != 3 {
				t.Fatal(s)
			}
			s = Evaluate(p, s, observation(now.Add(45*time.Second), 3, 500), 3)
			if s.Decision.DesiredReplicas != 3 {
				t.Fatal("uncertainty did not reset history", s)
			}
		})
	}
}
func TestReplayGapPolicyEditAndClockRewind(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	s := Evaluate(p, State{}, observation(now, 3, 500), 3)
	replay := observation(now, 3, 500)
	replay.At = now.Add(15 * time.Second)
	s = Evaluate(p, s, replay, 3)
	if s.Decision.Reason != "RepeatedSamples" {
		t.Fatal(s)
	}
	s = Evaluate(p, s, observation(now.Add(time.Minute*5), 3, 500), 3)
	if s.HighSamples != 1 {
		t.Fatal(s)
	}
	p.ScaleOutStep = 2
	s.LastAction = now
	s = Evaluate(p, s, observation(now.Add(time.Minute*5+15*time.Second), 3, 500), 3)
	if s.HighSamples != 1 || s.LastAction != now {
		t.Fatal(s)
	}
	s = Evaluate(p, s, observation(now.Add(time.Minute), 3, 500), 3)
	if s.Decision.Reason != "InvalidObservation" || s.Decision.DesiredReplicas != 3 {
		t.Fatal(s)
	}
}
func TestDeterministicPressureSignalsAndFloors(t *testing.T) {
	for _, signal := range []string{"cpu", "memory", "runtime", "backlog", "floor"} {
		t.Run(signal, func(t *testing.T) {
			p := policy()
			s := State{}
			now := time.Unix(10000, 0)
			if signal == "floor" {
				p.MinReplicas = 4
			}
			for i := range 3 {
				o := observation(now.Add(time.Duration(i)*15*time.Second), 3, 10)
				switch signal {
				case "cpu":
					o.Samples[0].CPU = 300
				case "memory":
					o.Samples[0].MemoryMiB = 900
				case "runtime":
					o.Samples[0].Pressured = true
				case "backlog":
					o.Samples[0].Backlog = true
				}
				a, b := Evaluate(p, s, o, 3), Evaluate(p, s, o, 3)
				ab, _ := json.Marshal(a)
				bb, _ := json.Marshal(b)
				if !bytes.Equal(ab, bb) {
					t.Fatal("not deterministic")
				}
				s = a
			}
			if s.Decision.DesiredReplicas != 4 {
				t.Fatal(s)
			}
		})
	}
}

func TestPolicyEditCannotExecuteCachedShadowRecommendation(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	s := State{}
	for i := range 3 {
		s = Evaluate(p, s, observation(now.Add(time.Duration(i)*15*time.Second), 3, 500), 3)
	}
	if s.Decision.DesiredReplicas != 4 {
		t.Fatal(s)
	}
	p.Mode = "ScaleOut"
	s = Evaluate(p, s, observation(now.Add(31*time.Second), 3, 500), 3)
	if s.Decision.DesiredReplicas != 3 || !s.HighSince.IsZero() {
		t.Fatal("cached recommendation executable after mode edit", s)
	}
}

func TestLowDemandCannotReplaceMissingObservations(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	o := observation(now, 3, 10)
	if !LowDemand(p, o, 3) {
		t.Fatal("low observation rejected")
	}
	for _, change := range []func(*Observation){
		func(o *Observation) { o.Complete = false }, func(o *Observation) { o.Samples[0].Pressured = true }, func(o *Observation) { o.Samples[0].Ready = false }, func(o *Observation) { o.Samples[0].RuntimeReceived = now.Add(-time.Hour) }, func(o *Observation) { o.Samples[0].MetricsAt = now.Add(time.Second) }, func(o *Observation) { o.Samples[0].Window = time.Hour }, func(o *Observation) { o.Samples[0].CPU = 100 },
	} {
		bad := observation(now, 3, 10)
		change(&bad)
		if LowDemand(p, bad, 3) {
			t.Fatal("uncertainty accepted", bad)
		}
	}
}

func TestFrequentReconcileKeepsDiagnosticButCannotAct(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	s := State{}
	for i := range 3 {
		s = Evaluate(p, s, observation(now.Add(time.Duration(i)*15*time.Second), 3, 500), 3)
	}
	if !s.Actionable {
		t.Fatal(s)
	}
	prior := s.Decision
	s = Evaluate(p, s, observation(now.Add(31*time.Second), 3, 500), 3)
	if s.Actionable || s.Decision != prior {
		t.Fatal("cached action or flickering status", s)
	}
	o := observation(now.Add(45*time.Second), 3, 500)
	o.Samples[0].Ready = false
	s = Evaluate(p, s, o, 3)
	prior = s.Decision
	o.At = o.At.Add(time.Second)
	s = Evaluate(p, s, o, 3)
	if s.Decision != prior || s.Decision.Reason != "PendingCapacity" {
		t.Fatal("blocker lost between intervals", s)
	}
}

func TestIntermediateObservationsInvalidateStabilization(t *testing.T) {
	for _, direction := range []string{"out", "in"} {
		for _, interruption := range []string{"opposite demand", "neutral demand", "missing metrics", "partial inventory", "unready", "stale runtime"} {
			t.Run(direction+"/"+interruption, func(t *testing.T) {
				p := policy()
				p.MinReplicas = 1
				p.ScaleInStabilizationSeconds = 60
				now := time.Unix(10000, 0)
				cpu := int64(500)
				last := 15
				if direction == "in" {
					cpu = 10
					last = 45
				}
				s := State{}
				for second := 0; second <= last; second += 15 {
					s = Evaluate(p, s, observation(now.Add(time.Duration(second)*time.Second), 3, cpu), 3)
				}
				accepted := s.LastObservation
				o := observation(now.Add(time.Duration(last+5)*time.Second), 3, cpu)
				switch interruption {
				case "opposite demand":
					for i := range o.Samples {
						o.Samples[i].CPU = 510 - cpu
					}
				case "neutral demand":
					for i := range o.Samples {
						o.Samples[i].CPU = 100
					}
				case "missing metrics":
					o.Samples[0].MetricsAt = time.Time{}
				case "partial inventory":
					o.Complete = false
				case "unready":
					o.Samples[0].Ready = false
				case "stale runtime":
					o.Samples[0].RuntimeAt = now.Add(-time.Hour)
				}
				s = Evaluate(p, s, o, 3)
				if s.Actionable || !s.HighSince.IsZero() || !s.LowSince.IsZero() {
					t.Error("intermediate contrary/unknown observation retained stabilization", s)
				}
				if s.LastObservation != accepted {
					t.Error("intermediate observation consumed the sample interval")
				}
				s = Evaluate(p, s, observation(now.Add(time.Duration(last+15)*time.Second), 3, cpu), 3)
				if s.Actionable || s.Decision.DesiredReplicas != 3 {
					t.Fatal("acted across an interrupted window", s)
				}
				// A complete new window can still recover; the interruption is not a sticky block.
				for second := last + 30; second <= last+75; second += 15 {
					s = Evaluate(p, s, observation(now.Add(time.Duration(second)*time.Second), 3, cpu), 3)
				}
				if !s.Actionable {
					t.Fatal("fresh stable window never recovered", s)
				}
			})
		}
	}
}

func TestIntermediateSupportingSamplesDoNotAdvanceWindow(t *testing.T) {
	p := policy()
	now := time.Unix(10000, 0)
	s := Evaluate(p, State{}, observation(now, 3, 500), 3)
	prior := s
	for second := 1; second < 15; second++ {
		s = Evaluate(p, s, observation(now.Add(time.Duration(second)*time.Second), 3, 500), 3)
	}
	if s.Actionable || s.HighSamples != prior.HighSamples || s.HighSince != prior.HighSince || s.LastObservation != prior.LastObservation || s.Stamps["0"] != prior.Stamps["0"] {
		t.Fatal("intermediate samples advanced the window", s)
	}
}
