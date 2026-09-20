package catalog

import (
	"os"
	"testing"

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
