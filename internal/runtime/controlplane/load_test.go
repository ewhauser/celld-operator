package controlplane

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	b, err := os.ReadFile("testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Load struct {
			Sampled int64 `json:"sampled_ms"`
		} `json:"node_load"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(raw.Load.Sampled).Add(time.Second)
	s, err := parseLoad(b, now, now, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.SampledAt.UnixMilli() != raw.Load.Sampled {
		t.Fatal("sample changed")
	}
	for _, field := range []string{"capacity_waiting", "activation_waiting", "restoring", "occupied", "owned_cells", "node_load"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []any{nil, "unknown", -1} {
				bad := mutate(t, b, func(m map[string]any) { m[field] = value })
				if _, err := parseLoad(bad, now, now, 5*time.Second); err == nil {
					t.Fatal("accepted invalid observation")
				}
			}
		})
	}
	for _, field := range []string{"draining", "rebalance_paused", "pressured", "memory_headroom", "sampled_ms", "resident_cells", "rss_bytes", "in_use_bytes"} {
		bad := mutate(t, b, func(m map[string]any) { delete(m["node_load"].(map[string]any), field) })
		if _, err := parseLoad(bad, now, now, 5*time.Second); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	for _, tc := range []struct {
		received, at time.Time
		age          time.Duration
	}{
		{now.Add(-time.Minute), now, time.Second},
		{now, now.Add(time.Minute), time.Second}, {now, now, 0},
		{now, now.Add(-time.Minute), time.Second},
	} {
		if _, err := parseLoad(b, tc.received, tc.at, tc.age); err == nil {
			t.Fatal("accepted stale/unavailable state")
		}
	}
}
func mutate(t *testing.T, data []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
