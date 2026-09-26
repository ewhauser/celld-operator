package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func applicationWire() map[string]any {
	return map[string]any{"deployment": map[string]any{"version": "v2", "prefix": "deploy/v2/", "generation": 2, "swapping": 0, "cells": map[string]any{"cell-a": 2, "cell-b": 1}}}
}
func TestApplicationReadOnlyContract(t *testing.T) {
	now := time.Now()
	data, _ := json.Marshal(applicationWire())
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/state" {
			t.Error("not an existing read-only observation", r.Method, r.URL)
		}
		writeJSON(w, string(data))
	})
	a, err := c.Application(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Fresh(time.Now(), now.Add(-time.Minute), 90*time.Second) || a.Loaded.Version != "v2" || a.PendingCells != 1 || a.ResidentCells != 2 {
		t.Fatalf("valid observation lost: %+v", a)
	}
}
func TestApplicationRejectsMalformedOrUnsupportedSnapshots(t *testing.T) {
	for _, scenario := range []string{"missing deployment", "null deployment", "missing cells", "null cells", "missing swapping", "null swapping", "negative swapping", "excessive swapping", "oversized version", "missing generation", "zero generation", "future cell generation", "null cell generation", "negative cell generation", "empty prefix"} {
		t.Run(scenario, func(t *testing.T) {
			m := applicationWire()
			d := m["deployment"].(map[string]any)
			switch scenario {
			case "missing deployment":
				delete(m, "deployment")
			case "null deployment":
				m["deployment"] = nil
			case "missing cells":
				delete(d, "cells")
			case "null cells":
				d["cells"] = nil
			case "missing swapping":
				delete(d, "swapping")
			case "null swapping":
				d["swapping"] = nil
			case "negative swapping":
				d["swapping"] = -1
			case "excessive swapping":
				d["swapping"] = 1e12
			case "oversized version":
				d["version"] = strings.Repeat("a", 257)
			case "missing generation":
				delete(d, "generation")
			case "zero generation":
				d["generation"] = 0
			case "future cell generation":
				d["cells"] = map[string]any{"cell": 3}
			case "null cell generation":
				d["cells"] = map[string]any{"cell": nil}
			case "negative cell generation":
				d["cells"] = map[string]any{"cell": -1}
			case "empty prefix":
				d["prefix"] = ""
			}
			data, _ := json.Marshal(m)
			if _, err := decodeApplication(data, time.Now()); err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}
func TestApplicationFreshness(t *testing.T) {
	now := time.Now()
	data, _ := json.Marshal(applicationWire())
	good, err := decodeApplication(data, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"old response", "future response", "previous pod", "missing deployment"} {
		t.Run(scenario, func(t *testing.T) {
			a := good
			started := now.Add(-time.Minute)
			switch scenario {
			case "old response":
				a.ReceivedAt = now.Add(-2 * time.Minute)
			case "future response":
				a.ReceivedAt = now.Add(time.Minute)
			case "previous pod":
				started = now.Add(time.Second)
			case "missing deployment":
				a.Loaded = nil
			}
			if a.Fresh(now, started, 90*time.Second) {
				t.Fatal("stale observation fresh")
			}
		})
	}
}
func TestApplicationLocalGenerationsAndOptionalIdentity(t *testing.T) {
	m := applicationWire()
	d := m["deployment"].(map[string]any)
	d["cells"] = map[string]any{}
	m["shutdown"] = map[string]any{"runtime_generation": "process-a"}
	data, _ := json.Marshal(m)
	a, err := decodeApplication(data, time.Now())
	if err != nil || a.PendingCells != 0 || a.RuntimeGeneration != "process-a" {
		t.Fatalf("empty resident census: %+v, %v", a, err)
	}
	oversized, _ := json.Marshal(map[string]any{"deployment": d, "shutdown": map[string]any{"runtime_generation": strings.Repeat("a", 129)}})
	if _, err := decodeApplication(oversized, time.Now()); !errors.Is(err, ErrIdentity) {
		t.Fatalf("accepted oversized identity: %v", err)
	}
	d["cells"] = map[string]any{"unknown": 0}
	data, _ = json.Marshal(m)
	a, err = decodeApplication(data, time.Now())
	if err != nil || a.PendingCells != 1 {
		t.Fatalf("unknown cell generation must remain pending: %+v, %v", a, err)
	}
}

// Captured with hack/test-application-state.py against celld c91ca54, without
// the proposed application observation patch, and disposable MinIO.
func TestApplicationNativeRuntimeFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/application-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples map[string]json.RawMessage
	if err := json.Unmarshal(data, &samples); err != nil {
		t.Fatal(err)
	}
	for name, data := range samples {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			a, err := decodeApplication(data, now)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Fresh(now, now.Add(-time.Hour), 90*time.Second) {
				t.Fatal("native observation failed freshness")
			}
			if name == "transition" && a.PendingCells == 0 {
				t.Fatal("pending resident cells lost")
			}
			if name != "transition" && a.PendingCells != 0 {
				t.Fatal("unexpected pending cells")
			}
		})
	}
}
