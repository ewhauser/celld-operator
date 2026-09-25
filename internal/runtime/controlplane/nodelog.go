package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNoNodeLog means the runtime reported no node-log state: a build older
// than 0.5.1-ewhauser.5, or a process without a node-log manager. It is
// unknown fleet health, never healthy.
var ErrNoNodeLog = errors.New("runtime reports no node-log state")

// NodeLog is celld's `/state.node_log`: this node's durability posture, its own
// folded log, shipper health, and the last dead-leader sweep's fleet view.
type NodeLog struct {
	// Posture is "fleet", "bucket", or empty before the durability stack runs.
	Posture        string
	Session        string
	Own            *OwnLog
	ShipperHealthy bool
	// Fleet is nil in bucket posture and before the first sweep pass.
	Fleet *FleetLog
}

type OwnLog struct {
	State          string
	Epoch          uint64
	Ensemble       []string
	BucketComplete bool
	Active         bool
}

// FleetLog is one sweep pass. Complete is false when the pass could not list
// or read every node record; its lists then hold only what it read.
type FleetLog struct {
	ObservedAt  time.Time
	Complete    bool
	Unrecovered []UnrecoveredLog
	// Obligations maps a member node to the leader sessions whose current
	// epoch still needs that member's fragment.
	Obligations map[string][]string
}

type UnrecoveredLog struct {
	Session, State string
	LeaseExpiresAt time.Time
	Claimant       string
}

type nodeLogWire struct {
	Posture *string `json:"posture"`
	Session string  `json:"session"`
	Own     *struct {
		State          string   `json:"state"`
		Epoch          uint64   `json:"epoch"`
		Ensemble       []string `json:"ensemble"`
		BucketComplete bool     `json:"bucket_complete"`
		Active         bool     `json:"active"`
	} `json:"own"`
	ShipperHealthy *bool `json:"shipper_healthy"`
	Fleet          *struct {
		ObservedMS  *int64 `json:"observed_ms"`
		Complete    *bool  `json:"complete"`
		Unrecovered []struct {
			Session        string  `json:"session"`
			State          string  `json:"state"`
			LeaseExpiresMS int64   `json:"lease_expires_ms"`
			Claimant       *string `json:"claimant"`
		} `json:"unrecovered"`
		Obligations map[string][]string `json:"obligations"`
	} `json:"fleet"`
}

// NodeLog decodes `/state.node_log`. Required fields must be present: a
// partial object is an error, not a healthy default.
func (s State) NodeLog() (NodeLog, error) {
	m, err := decodeObject(s.raw)
	if err != nil {
		return NodeLog{}, err
	}
	raw, ok := m["node_log"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return NodeLog{}, ErrNoNodeLog
	}
	var w nodeLogWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return NodeLog{}, fmt.Errorf("invalid node_log: %w", err)
	}
	if w.Session == "" || w.ShipperHealthy == nil {
		return NodeLog{}, errors.New("incomplete node_log")
	}
	out := NodeLog{Session: w.Session, ShipperHealthy: *w.ShipperHealthy}
	if w.Posture != nil {
		out.Posture = *w.Posture
	}
	if o := w.Own; o != nil {
		out.Own = &OwnLog{State: o.State, Epoch: o.Epoch, Ensemble: o.Ensemble, BucketComplete: o.BucketComplete, Active: o.Active}
	}
	if f := w.Fleet; f != nil {
		if f.ObservedMS == nil || f.Complete == nil || f.Obligations == nil {
			return NodeLog{}, errors.New("incomplete node_log fleet view")
		}
		view := &FleetLog{ObservedAt: time.UnixMilli(*f.ObservedMS), Complete: *f.Complete, Obligations: f.Obligations}
		for _, u := range f.Unrecovered {
			if u.Session == "" {
				return NodeLog{}, errors.New("unrecovered log without session")
			}
			log := UnrecoveredLog{Session: u.Session, State: u.State, LeaseExpiresAt: time.UnixMilli(u.LeaseExpiresMS)}
			if u.Claimant != nil {
				log.Claimant = *u.Claimant
			}
			view.Unrecovered = append(view.Unrecovered, log)
		}
		out.Fleet = view
	}
	return out, nil
}

// NodeLog reads one node's `/state.node_log`.
func (c *Client) NodeLog(ctx context.Context, target Target) (NodeLog, error) {
	state, err := c.State(ctx, target)
	if err != nil {
		return NodeLog{}, err
	}
	return state.NodeLog()
}
