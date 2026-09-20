package launcher

import (
	"os"
	"testing"
)

func TestRetiredPodCannotRestartButNewPodCanReuseDisk(t *testing.T) {
	c := config(t)
	cancel := startSupervisor(t, c)
	first := awaitPhase(t, c.Address, c.Key, "Running")
	if _, err := query(t, c.Address, c.Key, "retirement", first.Generation); err != nil {
		t.Fatal(err)
	}
	receipt := awaitPhase(t, c.Address, c.Key, "Stopped")
	if !receipt.RemovalReady() {
		t.Fatal("stopped receipt lacks durable restart denial")
	}
	marker, err := os.ReadFile(retiredPodPath(c.Root, c.PodUID))
	if err != nil || string(marker) != c.PodUID {
		t.Fatalf("missing durable deny marker: %s %v", marker, err)
	}
	cancel()
	stale := c
	stale.Address = testAddress(t)
	stopStale := startSupervisor(t, stale)
	state := awaitPhase(t, stale.Address, c.Key, "Blocked")
	if state.PID != 0 || state.Operation != "" || state.RuntimeDataSafe() || state.ChildExited || state.InheritedLockReleased {
		t.Fatal("disk marker reconstructed positive stopped authority or started child")
	}
	stopStale()
	next := c
	next.Address = testAddress(t)
	next.PodUID = "new-pod"
	next.Control = newStrictRuntime(t, next).client
	stopNext := startSupervisor(t, next)
	state = awaitPhase(t, next.Address, c.Key, "Running")
	if state.Generation == first.Generation {
		t.Fatal("reuse inherited retired generation")
	}
	if _, err := query(t, next.Address, c.Key, "retire-successor", state.Generation); err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, next.Address, c.Key, "Stopped")
	stopNext()
	// Reusing the disk must never erase the original pod's negative authority.
	again := c
	again.Address = testAddress(t)
	startSupervisor(t, again)
	state = awaitPhase(t, again.Address, c.Key, "Blocked")
	if state.PID != 0 {
		t.Fatal("old pod resumed after a newer pod retired")
	}
}

func TestDenyMarkerAloneNeverReconstructsStop(t *testing.T) {
	c := config(t)
	if err := os.WriteFile(retiredPodPath(c.Root, c.PodUID), []byte(c.PodUID), 0o600); err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, c)
	state := awaitPhase(t, c.Address, c.Key, "Blocked")
	if state.PID != 0 || state.Operation != "" || state.RuntimeDataSafe() || state.ChildExited || state.InheritedLockReleased {
		t.Fatal("marker manufactured a stopped receipt")
	}
}
