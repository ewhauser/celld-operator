package capacity

import (
	"encoding/json"
	"fmt"
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

// Only improvement sustained over the window releases the hold and resets
// IneffectiveBatches. An addition that can no longer be judged is dropped
// without counting as improvement.
func TestRedistributionHoldRequiresSustainedImprovement(t *testing.T) {
	for _, tc := range []struct{ scenario, want string }{
		{"growth", "release"}, {"redistributed", "release"},
		{"transient", "hold"}, {"missing", "hold"}, {"intermediate", "hold"}, {"idlecache", "hold"},
		{"replacement", "drop"}, {"contraction", "drop"},
	} {
		scenario := tc.scenario
		t.Run(scenario, func(t *testing.T) {
			p := policy()
			now := time.Unix(10000, 0)
			before := loads(observation(now, 3, 0))
			v := before["0"]
			v.CPU = 400
			before["0"] = v
			s := State{IneffectiveBatches: 2, Addition: &Addition{Before: before, Target: 4}}
			n := int32(4)
			if scenario == "contraction" {
				n = 3
			}
			for i := range 15 {
				at := now.Add(time.Duration(i) * 15 * time.Second)
				o := observation(at, n, 0)
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
				s = Evaluate(p, s, o, n)
				if scenario == "intermediate" {
					o = observation(at.Add(5*time.Second), 4, 0)
					o.Samples[0].CPU = 400
					s = Evaluate(p, s, o, 4)
				}
			}
			got := "unexpected"
			switch {
			case s.Addition == nil && s.IneffectiveBatches == 0:
				got = "release"
			case s.Addition != nil && s.IneffectiveBatches == 2 && s.Decision.Reason == "LoadNotRedistributed":
				got = "hold"
			case s.Addition == nil && s.IneffectiveBatches == 2:
				got = "drop"
			}
			if got != tc.want {
				t.Fatalf("%s, want %s: %+v", got, tc.want, s)
			}
		})
	}
}

// An addition is judged against the incumbents it grew from. Once one of them
// is replaced, or the fleet contracts below the addition's target, no
// observation can judge it again. It is dropped without counting as relief or as an
// ineffective batch, and the next addition still waits for a new stable window
// and the cooldown from the last action.
func TestUnjudgeableAdditionIsDropped(t *testing.T) {
	for _, cause := range []string{"replacement", "contraction"} {
		for batches := range int32(3) {
			t.Run(fmt.Sprintf("%s/%d", cause, batches), func(t *testing.T) {
				p := policy()
				now := time.Unix(10000, 0)
				n := int32(4)
				if cause == "contraction" {
					n = 3
				}
				s := State{IneffectiveBatches: batches, LastAction: now, Addition: &Addition{Before: loads(observation(now, 3, 400)), Target: 4}}
				for i := 1; i <= 40 && !s.Actionable; i++ {
					o := observation(now.Add(time.Duration(i)*15*time.Second), n, 400)
					if cause == "replacement" {
						o.Samples[0].Identity = "replacement"
					}
					s = Evaluate(p, s, o, n)
					if s.Addition != nil || s.IneffectiveBatches != batches {
						t.Fatalf("an unjudgeable addition was kept or counted: %+v", s)
					}
					if i == 1 && s.Decision.Reason != "StabilizingOut" {
						t.Fatalf("the drop skipped a new stable window: %+v", s.Decision)
					}
				}
				if waited := s.LastObservation.Sub(now); s.Decision.Reason != "ScaleOutRecommended" || waited < Seconds(p.ScaleOutCooldownSeconds) {
					t.Fatalf("after %v: %+v", waited, s.Decision)
				}
			})
		}
	}
}

// An addition is judged once one outcome holds for a whole window. Evidence
// that has not settled after twice the observation a judgment needs is mixed:
// the addition counts as ineffective, never as relief. The bound covers
// continuous complete observation, so an incomplete observation restarts it.
func TestMixedRedistributionIsIneffective(t *testing.T) {
	// The newcomer alternates each minute either side of cpuLowMillicores.
	alternating := func(elapsed time.Duration) bool { return elapsed/time.Minute%2 == 0 }
	for _, tc := range []struct {
		name       string
		batches    int32
		busy       func(time.Duration) bool
		incomplete time.Duration
		sparse     bool
		// judged is when the addition is judged, zero if it stays held.
		judged time.Duration
		want   int32
	}{
		{name: "mixed/0", batches: 0, busy: alternating, judged: 240 * time.Second, want: 1},
		{name: "mixed/1", batches: 1, busy: alternating, judged: 240 * time.Second, want: 2},
		{name: "mixed/2", batches: 2, busy: alternating, want: 2},
		// Relief that starts within the first window still settles in time.
		{name: "late relief", batches: 1, busy: func(e time.Duration) bool { return e >= 90*time.Second }, judged: 210 * time.Second, want: 0},
		{name: "interrupted", batches: 0, busy: alternating, incomplete: 195 * time.Second, judged: 450 * time.Second, want: 1},
		// Twice the samples a judgment needs, not only twice the window.
		{name: "sparse", batches: 0, busy: func(e time.Duration) bool { return e/(45*time.Second)%2 == 0 }, sparse: true, judged: 225 * time.Second, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := policy()
			interval := 15 * time.Second
			if tc.sparse {
				p.RedistributionObservationSeconds = 30
				p.SampleIntervalSeconds = 45
				interval = 45 * time.Second
			}
			now := time.Unix(10000, 0)
			before := loads(observation(now, 3, 0))
			v := before["0"]
			v.CPU = 1000
			before["0"] = v
			s := State{IneffectiveBatches: tc.batches, Addition: &Addition{Before: before, Target: 4}}
			var judged time.Duration
			for i := range 480 {
				elapsed := time.Duration(i) * interval
				o := observation(now.Add(elapsed), 4, 0)
				o.Samples[0].CPU = 1000
				o.Samples[3].CPU = 70
				if tc.busy(elapsed) {
					o.Samples[3].CPU = 90
				}
				if tc.incomplete != 0 && elapsed == tc.incomplete {
					o.Complete = false
				}
				s = Evaluate(p, s, o, 4)
				if s.Addition == nil || s.IneffectiveBatches != tc.batches {
					judged = elapsed
					break
				}
			}
			if judged != tc.judged || s.IneffectiveBatches != tc.want || (s.Addition != nil) != (tc.want == 2) {
				t.Fatalf("judged at %v with %d ineffective, want %v with %d (0s is never): %+v", judged, s.IneffectiveBatches, tc.judged, tc.want, s)
			}
			if tc.want == 2 && s.Decision.Reason != "LoadNotRedistributed" {
				t.Fatalf("not held: %+v", s.Decision)
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
