package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"
)

// ApplicationReader observes deployments without reloading or modifying a node.
type ApplicationReader interface {
	Application(context.Context, Target) (Application, error)
}

type ApplicationVersion struct {
	Version string `json:"version"`
	Prefix  string `json:"prefix"`
}
type ApplicationTarget struct {
	ApplicationVersion
	ObservedAtMS int64 `json:"observed_at_ms"`
}
type Application struct {
	RuntimeGeneration string              `json:"runtime_generation"`
	SampledAtMS       int64               `json:"sampled_at_ms"`
	SnapshotValid     bool                `json:"snapshot_valid"`
	Loaded            *ApplicationVersion `json:"loaded"`
	LocalGeneration   uint64              `json:"local_generation"`
	Target            *ApplicationTarget  `json:"target"`
	PointerStatus     string              `json:"pointer_status"`
	AdoptionStatus    string              `json:"adoption_status"`
	ResidentCells     int64               `json:"resident_cells"`
	PendingCells      int64               `json:"pending_cells"`
	SwappingCells     int64               `json:"swapping_cells"`
	ReceivedAt        time.Time           `json:"-"`
}

func (c *Client) Application(ctx context.Context, target Target) (Application, error) {
	data, code, err := c.call(ctx, target, http.MethodGet, "/state?view=application", nil)
	if err != nil {
		return Application{}, err
	}
	if code != http.StatusOK {
		return Application{}, errors.New("unexpected application state status")
	}
	return decodeApplication(data, target.Generation, time.Now())
}

func decodeApplication(data []byte, generation string, received time.Time) (Application, error) {
	m, err := decodeObject(data)
	if err != nil {
		return Application{}, err
	}
	var version uint32
	if err := required(m, "schema_version", &version); err != nil || version != 1 {
		return Application{}, ErrUnsupported
	}
	var out Application
	// Explicit zero counts and false validity are distinct from omitted fields.
	for k, p := range map[string]any{
		"runtime_generation": &out.RuntimeGeneration, "sampled_at_ms": &out.SampledAtMS,
		"snapshot_valid": &out.SnapshotValid, "local_generation": &out.LocalGeneration,
		"pointer_status": &out.PointerStatus, "adoption_status": &out.AdoptionStatus,
		"resident_cells": &out.ResidentCells, "pending_cells": &out.PendingCells, "swapping_cells": &out.SwappingCells,
	} {
		if err := required(m, k, p); err != nil {
			return Application{}, err
		}
	}
	for _, k := range []string{"loaded", "target"} {
		if _, ok := m[k]; !ok {
			return Application{}, errors.New("incomplete deployment snapshot")
		}
	}
	if err := json.Unmarshal(m["loaded"], &out.Loaded); err != nil {
		return Application{}, err
	}
	if err := json.Unmarshal(m["target"], &out.Target); err != nil {
		return Application{}, err
	}
	if out.RuntimeGeneration == "" || len(out.RuntimeGeneration) > 128 || (generation != "" && generation != out.RuntimeGeneration) {
		return Application{}, ErrIdentity
	}
	validVersion := func(v *ApplicationVersion) bool {
		return v != nil && v.Version != "" && len(v.Version) <= 256 && v.Prefix != "" && len(v.Prefix) <= 1024
	}
	if (out.Loaded != nil && !validVersion(out.Loaded)) || (out.Target != nil && (!validVersion(&out.Target.ApplicationVersion) || out.Target.ObservedAtMS <= 0)) ||
		(out.SnapshotValid && (out.Loaded == nil || out.LocalGeneration == 0)) || (out.PointerStatus == "observed" && out.Target == nil) ||
		!slices.Contains([]string{"unknown", "observed", "unavailable"}, out.PointerStatus) ||
		!slices.Contains([]string{"unknown", "adopting", "adopted", "unchanged", "failed"}, out.AdoptionStatus) ||
		out.ResidentCells < 0 || out.ResidentCells > 1_000_000_000 || out.PendingCells < 0 || out.PendingCells > out.ResidentCells || out.SwappingCells < 0 || out.SwappingCells > 1_000_000_000 {
		return Application{}, errors.New("invalid deployment snapshot")
	}
	out.ReceivedAt = received
	return out, nil
}

// Freshness covers both the node snapshot and its last successful pointer read.
// A recent HTTP response cannot make an old deployment target current again.
func (a Application) Fresh(now, started time.Time, maxAge time.Duration) bool {
	sample := time.UnixMilli(a.SampledAtMS)
	return a.SnapshotValid && a.Target != nil && a.PointerStatus == "observed" &&
		fresh(a.ReceivedAt, now, maxAge) && fresh(sample, now, maxAge) && !sample.Before(started) &&
		fresh(time.UnixMilli(a.Target.ObservedAtMS), now, maxAge) && !time.UnixMilli(a.Target.ObservedAtMS).Before(started) && a.Target.ObservedAtMS <= a.SampledAtMS
}
