package launcher

import (
	"os"
	"syscall"
	"testing"
)

func TestRetiredDiskRejectsEveryPodIdentity(t *testing.T) {
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
	marker, err := os.ReadFile(retiredDiskPath(c.Root))
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
	state = awaitPhase(t, next.Address, c.Key, "Blocked")
	if state.PID != 0 || state.RemovalReady() {
		t.Fatal("replacement pod reopened a disk covered by captured removal proof")
	}
	stopNext()
	// A replacement may run only with a fresh disk, as scale-out provides.
	fresh := next
	fresh.Root = t.TempDir()
	fresh.Address = testAddress(t)
	fresh.Control = newStrictRuntime(t, fresh).client
	startSupervisor(t, fresh)
	state = awaitPhase(t, fresh.Address, c.Key, "Running")
	if state.Generation == first.Generation || state.DiskID == first.DiskID {
		t.Fatal("fresh disk inherited a retired runtime identity")
	}
}

func TestDenyMarkerAloneNeverReconstructsStop(t *testing.T) {
	c := config(t)
	if err := os.WriteFile(retiredDiskPath(c.Root), []byte("another-pod"), 0o600); err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, c)
	state := awaitPhase(t, c.Address, c.Key, "Blocked")
	if state.PID != 0 || state.Operation != "" || state.RuntimeDataSafe() || state.ChildExited || state.InheritedLockReleased {
		t.Fatal("marker manufactured a stopped receipt")
	}
}

func TestDiskDeniedBeforeInheritedLockProof(t *testing.T) {
	c := config(t)
	// Avoid os/exec output-copy pipes: Wait must observe the direct child's
	// exit independently of a descendant holding stdout and the lock FD.
	c.Stdout, c.Stderr = nil, nil
	ready, command := shellFixture(t)
	c.Command = command("trap 'exit 0' TERM\nsleep 600 &")
	startSupervisor(t, c)
	awaitReady(t, ready)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	t.Cleanup(func() { _ = syscall.Kill(-running.PID, syscall.SIGKILL) })
	if _, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil {
		t.Fatal(err)
	}
	state := awaitPhase(t, c.Address, c.Key, "ReleasingInheritedLock")
	if !state.ChildExited || !state.RestartDenied || state.InheritedLockReleased || state.RemovalReady() {
		t.Fatalf("deny marker and inherited-lock proof conflated: %+v", state)
	}
	if _, err := os.Stat(retiredDiskPath(c.Root)); err != nil {
		t.Fatalf("disk could reopen during independent lock acquisition: %v", err)
	}
	_ = syscall.Kill(-running.PID, syscall.SIGKILL)
	if state := awaitPhase(t, c.Address, c.Key, "Stopped"); !state.RemovalReady() {
		t.Fatal("complete strict stop did not produce all independent proofs")
	}
}
