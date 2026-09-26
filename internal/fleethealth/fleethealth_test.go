package fleethealth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
)

var horizon = time.UnixMilli(1_790_000_000_000)

func healthy(node string, obligations map[string][]string) Member {
	return Member{Node: node, Ready: true, Log: &controlplane.NodeLog{
		Posture: "fleet", Session: node + "/g", ShipperHealthy: true,
		Fleet: &controlplane.FleetLog{ObservedAt: horizon.Add(time.Second), Complete: true, Obligations: obligations},
	}}
}

func fleet() []Member {
	return []Member{
		healthy("a-0", map[string][]string{"a-1": {"a-0/g"}}),
		healthy("a-1", map[string][]string{"a-0": {"a-1/g"}}),
		healthy("a-2", map[string][]string{}),
	}
}

func TestSettledFleet(t *testing.T) {
	a := Assess(fleet(), horizon)
	if !a.Settled || a.Reason != "" {
		t.Fatalf("healthy fleet not settled: %s", a.Reason)
	}
}

func TestUnsettledConditions(t *testing.T) {
	for name, c := range map[string]struct {
		edit func([]Member) []Member
		want string
	}{
		"unready":  {func(m []Member) []Member { m[1].Ready = false; return m }, "not ready"},
		"no state": {func(m []Member) []Member { m[1].Log, m[1].Err = nil, errors.New("503"); return m }, "unavailable: 503"},
		"old runtime": {func(m []Member) []Member {
			m[1].Log, m[1].Err = nil, controlplane.ErrNoNodeLog
			return m
		}, "no node-log state"},
		"bucket posture": {func(m []Member) []Member { m[0].Log.Posture = "bucket"; return m }, "not fleet"},
		"no ensemble":    {func(m []Member) []Member { m[2].Log.ShipperHealthy = false; return m }, "no follower ensemble"},
		"stale sweep": {func(m []Member) []Member {
			for i := range m {
				m[i].Log.Fleet.ObservedAt = horizon.Add(-time.Second)
			}
			return m
		}, "no complete fleet sweep"},
		"incomplete sweep": {func(m []Member) []Member {
			for i := range m {
				m[i].Log.Fleet.Complete = false
			}
			return m
		}, "no complete fleet sweep"},
		"unrecovered": {func(m []Member) []Member {
			m[2].Log.Fleet.Unrecovered = []controlplane.UnrecoveredLog{{Session: "a-3/g", State: "recovering"}}
			return m
		}, "a-3/g is recovering"},
		"no members": {func([]Member) []Member { return nil }, "no expected members"},
	} {
		t.Run(name, func(t *testing.T) {
			a := Assess(c.edit(fleet()), horizon)
			if a.Settled || !strings.Contains(a.Reason, c.want) {
				t.Fatalf("got settled=%v reason %q, want %q", a.Settled, a.Reason, c.want)
			}
		})
	}
}

func TestSingleMemberNeedsNoEnsemble(t *testing.T) {
	m := []Member{healthy("a-0", map[string][]string{})}
	m[0].Log.ShipperHealthy = false
	if a := Assess(m, horizon); !a.Settled {
		t.Fatalf("one-member fleet cannot have followers: %s", a.Reason)
	}
}

func TestStaleUnrecoveredIsIgnored(t *testing.T) {
	m := fleet()
	m[0].Log.Fleet.ObservedAt = horizon.Add(-time.Minute)
	m[0].Log.Fleet.Unrecovered = []controlplane.UnrecoveredLog{{Session: "old/g", State: "open"}}
	if a := Assess(m, horizon); !a.Settled {
		t.Fatalf("a sweep from before the horizon must not decide: %s", a.Reason)
	}
}

func TestReleasable(t *testing.T) {
	a := Assess(fleet(), horizon)
	if a.Releasable("a-0") || a.Releasable("a-1") {
		t.Fatal("a member named by a current ensemble is not releasable")
	}
	if !a.Releasable("a-2") || !a.Releasable("gone") {
		t.Fatal("a member no session needs is releasable")
	}
	sessions, ok := a.Obligations("a-1")
	if !ok || len(sessions) != 1 || sessions[0] != "a-0/g" {
		t.Fatalf("obligations %v %v", sessions, ok)
	}
}

func TestReleasableUnionsFreshViewsAndNeedsOne(t *testing.T) {
	m := fleet()
	m[2].Log.Fleet.Obligations = map[string][]string{"a-2": {"a-1/g"}}
	if Assess(m, horizon).Releasable("a-2") {
		t.Fatal("any fresh view's obligation blocks release")
	}
	for i := range m {
		m[i].Log.Fleet.Complete = false
	}
	if _, ok := Assess(m, horizon).Obligations("x"); ok {
		t.Fatal("no complete view means unknown")
	}
	if Assess(m, horizon).Releasable("x") {
		t.Fatal("unknown is not releasable")
	}
	// A removed member reports nothing; the survivors' views still decide.
	survivors := fleet()[:2]
	if !Assess(survivors, horizon).Releasable("a-2") {
		t.Fatal("survivor views decide release of a stopped member")
	}
}
