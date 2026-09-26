// Package fleethealth decides, from celld's own node-log reports, whether a
// PersistentFleet has absorbed its last disruption (ADR 0023). It is pure: the
// caller gathers observations and applies the answer.
//
// Two questions matter. A fleet is settled when every expected member is ready,
// runs fleet durability with a follower ensemble, and a complete sweep made
// after the last disruption finds no unrecovered session. A member is
// releasable when no fresh complete sweep lists a session that still needs its
// disk. Anything unknown answers no: waiting affects only the next voluntary
// step, never the running fleet.
package fleethealth

import (
	"fmt"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
)

// Member is one expected member as the operator observed it. Node is celld's
// node identity (the Pod name for PersistentFleet). Log is nil when `/state`
// could not be read or carried no node-log state; Err says why.
type Member struct {
	Node  string
	Ready bool
	Log   *controlplane.NodeLog
	Err   error
}

type Assessment struct {
	Settled bool
	// Reason explains the first unmet condition; empty when settled.
	Reason string
	// fresh holds complete sweep views observed at or after the horizon.
	fresh []*controlplane.FleetLog
}

// Assess evaluates members against horizon: the last disruption plus one lease
// TTL, so a crash whose lease has not yet expired cannot look recovered.
func Assess(members []Member, horizon time.Time) Assessment {
	a := Assessment{}
	for _, m := range members {
		if m.Log == nil || m.Log.Fleet == nil {
			continue
		}
		if f := m.Log.Fleet; f.Complete && !f.ObservedAt.Before(horizon) {
			a.fresh = append(a.fresh, f)
		}
	}
	a.Reason = a.unmet(members, horizon)
	a.Settled = a.Reason == ""
	return a
}

func (a Assessment) unmet(members []Member, horizon time.Time) string {
	if len(members) == 0 {
		return "no expected members"
	}
	for _, m := range members {
		switch {
		case !m.Ready:
			return fmt.Sprintf("member %s is not ready", m.Node)
		case m.Log == nil:
			return fmt.Sprintf("member %s node-log state unavailable: %v", m.Node, m.Err)
		case m.Log.Posture != "fleet":
			return fmt.Sprintf("member %s runs %q durability, not fleet", m.Node, m.Log.Posture)
		case len(members) > 1 && !m.Log.ShipperHealthy:
			return fmt.Sprintf("member %s has no follower ensemble", m.Node)
		}
	}
	if len(a.fresh) == 0 {
		return fmt.Sprintf("no complete fleet sweep since %s", horizon.UTC().Format(time.RFC3339))
	}
	for _, f := range a.fresh {
		if len(f.Unrecovered) > 0 {
			u := f.Unrecovered[0]
			return fmt.Sprintf("session %s is %s", u.Session, u.State)
		}
	}
	return ""
}

// Obligations lists the sessions any fresh complete sweep says still need
// node's disk. ok is false when no such sweep exists, so the answer is unknown.
func (a Assessment) Obligations(node string) (sessions []string, ok bool) {
	for _, f := range a.fresh {
		for _, s := range f.Obligations[node] {
			if !slices.Contains(sessions, s) {
				sessions = append(sessions, s)
			}
		}
	}
	slices.Sort(sessions)
	return sessions, len(a.fresh) > 0
}

// Releasable reports whether node's disk may be deleted: a fresh complete sweep
// exists and none lists a session that still needs it.
func (a Assessment) Releasable(node string) bool {
	sessions, ok := a.Obligations(node)
	return ok && len(sessions) == 0
}
