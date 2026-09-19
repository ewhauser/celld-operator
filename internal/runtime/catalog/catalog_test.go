package catalog

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	v041 "github.com/ewhauser/celld-operator/internal/runtime/v041"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

func TestReleasedV041Evidence(t *testing.T) {
	adapter, err := New(v041.Image)
	if err != nil {
		t.Fatal(err)
	}
	node, err := os.ReadFile("testdata/v041-node.json")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := adapter.ParseNode("nodes/a.json", node)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "a" || parsed.LogState != "open" || parsed.Epoch != 1 {
		t.Fatal(parsed)
	}
	state, err := os.ReadFile("testdata/v041-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Load struct {
			Sampled int64 `json:"sampled_ms"`
		} `json:"node_load"`
	}
	if err := json.Unmarshal(state, &capture); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(capture.Load.Sampled)
	if _, err := adapter.ParseState(200, state, now, now, time.Second); err != nil {
		t.Fatal(err)
	}
}
func TestDirectionalReleaseRegistry(t *testing.T) {
	if !StoppedUpgrade(v041.Image, v050.Image) || StoppedUpgrade(v050.Image, v041.Image) || StoppedUpgrade(v050.Image, v050.Image) {
		t.Fatal("incorrect directional contract")
	}
	for _, image := range []string{"ghcr.io/denoland/celld:v0.4.1", v050.ARM64, "unknown"} {
		if _, err := New(image); err == nil {
			t.Fatal("accepted unqualified deployment identity", image)
		}
	}
}
