package capacity

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHotCellStopsIneffectiveAdditions(t *testing.T) {
	p := policy()
	p.Mode = "ScaleOut"
	s := State{}
	n := int32(3)
	start := time.Unix(10000, 0)
	for i := range 160 {
		at := start.Add(time.Duration(i) * 15 * time.Second)
		o := observation(at, n, 0)
		o.Samples[0].CPU = 400
		s = Evaluate(p, s, o, n)
		if s.Actionable && s.Decision.DesiredReplicas > n {
			next := s.Decision.DesiredReplicas
			RecordAction(&s, at, n, next)
			n = next
		}
		// Every observation passes through durable JSON, like leader reconstruction.
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
	}
	if n != 5 || s.Decision.Reason != "LoadNotRedistributed" {
		t.Fatalf("unchanged hot pod grew fleet to %d: %+v", n, s.Decision)
	}
}

func TestRedistributionHoldRequiresSustainedImprovement(t *testing.T) {
	for _, scenario := range []string{"growth", "redistributed", "transient", "missing", "intermediate", "replacement", "idlecache"} {
		t.Run(scenario, func(t *testing.T) {
			p := policy()
			now := time.Unix(10000, 0)
			before := loads(observation(now, 3, 0))
			v := before["0"]
			v.CPU = 400
			before["0"] = v
			s := State{IneffectiveBatches: 2, Addition: &Addition{Before: before, Target: 4}}
			for i := range 15 {
				at := now.Add(time.Duration(i) * 15 * time.Second)
				o := observation(at, 4, 0)
				o.Samples[0].CPU = 600
				if scenario == "redistributed" {
					o.Samples[0].CPU = 300
					o.Samples[3].CPU = 100
				}
				if scenario == "transient" && i != 1 {
					o.Samples[0].CPU = 400
				}
				if scenario == "missing" && i%3 == 0 {
					o.Complete = false
				}
				if scenario == "idlecache" {
					o.Samples[0].CPU = 400
					o.Samples[3].MemoryMiB = 600
				}
				if scenario == "replacement" {
					o.Samples[0].Identity = "replacement"
				}
				s = Evaluate(p, s, o, 4)
				if scenario == "intermediate" {
					o = observation(at.Add(5*time.Second), 4, 0)
					o.Samples[0].CPU = 400
					s = Evaluate(p, s, o, 4)
				}
			}
			release := scenario == "growth" || scenario == "redistributed"
			if release != (s.Addition == nil && s.IneffectiveBatches == 0) {
				t.Fatalf("unexpected hold result: %+v", s)
			}
		})
	}
}
func TestRedistributionDoesNotBlockMinimum(t *testing.T) {
	p := policy()
	p.MinReplicas = 6
	s := State{}
	n := int32(3)
	start := time.Unix(10000, 0)
	for i := range 100 {
		at := start.Add(time.Duration(i) * 15 * time.Second)
		s = Evaluate(p, s, observation(at, n, 0), n)
		if s.Actionable && s.Decision.DesiredReplicas > n {
			next := s.Decision.DesiredReplicas
			RecordAction(&s, at, n, next)
			n = next
		}
	}
	if n != 6 {
		t.Fatalf("minimum stranded at %d", n)
	}
}
