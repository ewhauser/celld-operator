package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Load struct {
	CapacityWaiting, ActivationWaiting, Restoring, Occupied, OwnedCells uint64
	ResidentCells, RSSBytes, InUseBytes                                 uint64
	Draining, RebalancePaused, Pressured, MemoryHeadroom                bool
	SampledAt                                                           time.Time
}

// parseLoad validates required capacity fields; optional cgroup measurements
// are intentionally not converted to zero. The caller validates HTTP status.
func parseLoad(data []byte, received, now time.Time, age time.Duration) (Load, error) {
	var s Load
	if !fresh(received, now, age) {
		return s, errors.New("unavailable or stale HTTP state")
	}
	m, err := decodeObject(data)
	if err != nil {
		return s, err
	}
	for k, p := range map[string]*uint64{"capacity_waiting": &s.CapacityWaiting, "activation_waiting": &s.ActivationWaiting, "restoring": &s.Restoring, "occupied": &s.Occupied, "owned_cells": &s.OwnedCells} {
		if err := required(m, k, p); err != nil {
			return s, err
		}
	}
	load, err := decodeObject(m["node_load"])
	if err != nil {
		return s, err
	}
	var sampled int64
	for k, p := range map[string]any{"draining": &s.Draining, "rebalance_paused": &s.RebalancePaused, "sampled_ms": &sampled, "resident_cells": &s.ResidentCells, "rss_bytes": &s.RSSBytes, "in_use_bytes": &s.InUseBytes, "pressured": &s.Pressured, "memory_headroom": &s.MemoryHeadroom} {
		if err := required(load, k, p); err != nil {
			return s, err
		}
	}
	s.SampledAt = time.UnixMilli(sampled)
	if !fresh(s.SampledAt, now, age) {
		return s, errors.New("stale or future runtime sample")
	}
	return s, nil
}

func required(m map[string]json.RawMessage, key string, target any) error {
	raw, ok := m[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("missing %s", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid %s: %w", key, err)
	}
	return nil
}
func fresh(sample, now time.Time, age time.Duration) bool {
	return age > 0 && !sample.IsZero() && !sample.After(now) && now.Sub(sample) <= age
}
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if err := validObject(data); err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}
