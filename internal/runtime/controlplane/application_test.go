package controlplane

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func applicationWire(now time.Time) map[string]any {
	return map[string]any{"schema_version": 1, "runtime_generation": "generation-a", "sampled_at_ms": now.UnixMilli(), "snapshot_valid": true,
		"loaded": map[string]any{"version": "v2", "prefix": "deploy/v2/"}, "local_generation": 2,
		"target":         map[string]any{"version": "v2", "prefix": "deploy/v2/", "observed_at_ms": now.UnixMilli()},
		"pointer_status": "observed", "adoption_status": "adopted", "resident_cells": 3, "pending_cells": 0, "swapping_cells": 0}
}
func TestApplicationReadOnlyContract(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	data, _ := json.Marshal(applicationWire(now))
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/state?view=application" {
			t.Error("not a read-only compact observation", r.Method, r.URL)
		}
		writeJSON(w, string(data))
	})
	a, err := c.Application(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Fresh(time.Now(), now.Add(-time.Minute), 90*time.Second) || a.Loaded.Version != "v2" {
		t.Fatalf("valid snapshot lost: %+v", a)
	}
	if _, err := c.Application(t.Context(), Target{IP: target.IP, Generation: "another-process"}); err == nil {
		t.Fatal("accepted wrong incarnation")
	}
}
func TestApplicationRejectsMalformedOrUnsupportedSnapshots(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	for _, scenario := range []string{"missing count", "null count", "negative", "excessive", "unknown schema", "missing schema", "empty identity", "missing target", "bad target", "bad enum", "oversized version", "invalid pending", "missing local generation"} {
		t.Run(scenario, func(t *testing.T) {
			m := applicationWire(now)
			switch scenario {
			case "missing count":
				delete(m, "pending_cells")
			case "null count":
				m["pending_cells"] = nil
			case "negative":
				m["pending_cells"] = -1
			case "excessive":
				m["resident_cells"] = 1e12
			case "unknown schema":
				m["schema_version"] = 2
			case "missing schema":
				delete(m, "schema_version")
			case "empty identity":
				m["runtime_generation"] = ""
			case "missing target":
				delete(m, "target")
			case "bad target":
				m["target"] = nil
			case "bad enum":
				m["adoption_status"] = "successful"
			case "oversized version":
				m["loaded"].(map[string]any)["version"] = strings.Repeat("a", 257)
			case "invalid pending":
				m["pending_cells"] = 4
			case "missing local generation":
				delete(m, "local_generation")
			}
			data, _ := json.Marshal(m)
			if _, err := decodeApplication(data, "", now); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}
func TestApplicationFreshnessRequiresBothObservations(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	data, _ := json.Marshal(applicationWire(now))
	good, err := decodeApplication(data, "", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"old target", "future target", "old sample", "future sample", "old response", "previous pod", "inconsistent snapshot", "pointer failure"} {
		t.Run(scenario, func(t *testing.T) {
			a := good
			target := *a.Target
			a.Target = &target
			started := now.Add(-time.Minute)
			switch scenario {
			case "old target":
				a.Target.ObservedAtMS = now.Add(-2 * time.Minute).UnixMilli()
			case "future target":
				a.Target.ObservedAtMS = now.Add(time.Minute).UnixMilli()
			case "old sample":
				a.SampledAtMS = now.Add(-2 * time.Minute).UnixMilli()
			case "future sample":
				a.SampledAtMS = now.Add(time.Minute).UnixMilli()
			case "old response":
				a.ReceivedAt = now.Add(-2 * time.Minute)
			case "previous pod":
				started = now.Add(time.Second)
			case "inconsistent snapshot":
				a.SnapshotValid = false
			case "pointer failure":
				a.PointerStatus = "unavailable"
			}
			if a.Fresh(now, started, 90*time.Second) {
				t.Fatal("stale observation fresh")
			}
		})
	}
}

// Captured from scripts/test-application-status.py against the real native runtime
// and disposable MinIO. Preserve runtime serialization, including flattened targets.
func TestApplicationNativeRuntimeFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/application-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples map[string]json.RawMessage
	if err := json.Unmarshal(data, &samples); err != nil {
		t.Fatal(err)
	}
	for name, data := range samples {
		t.Run(name, func(t *testing.T) {
			var timestamp struct {
				SampledAtMS int64 `json:"sampled_at_ms"`
			}
			if err := json.Unmarshal(data, &timestamp); err != nil {
				t.Fatal(err)
			}
			now := time.UnixMilli(timestamp.SampledAtMS)
			a, err := decodeApplication(data, "", now)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Fresh(now, now.Add(-time.Hour), 90*time.Second) {
				t.Fatal("native snapshot failed freshness")
			}
			if name == "transition" && a.PendingCells == 0 {
				t.Fatal("pending resident cells lost")
			}
			if name == "failure" && (a.AdoptionStatus != "failed" || a.Loaded.Version == a.Target.Version) {
				t.Fatal("failed adoption hid previous serving version")
			}
		})
	}
}
