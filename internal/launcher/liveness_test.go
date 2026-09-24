package launcher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLivenessOnlyFailsUnexpectedExit(t *testing.T) {
	for _, phase := range []string{"WaitingForExclusiveVolume", "Spacing", "Running", "Draining", "Terminating", "ReleasingInheritedLock", "Stopped", "Failed", "Blocked", "ExitedUnrequested"} {
		t.Run(phase, func(t *testing.T) {
			s := &supervisor{state: State{Phase: phase}}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/livez", nil))
			want := http.StatusOK
			if phase == "ExitedUnrequested" {
				want = http.StatusServiceUnavailable
			}
			if rec.Code != want {
				t.Fatalf("liveness in %s: got %d, want %d", phase, rec.Code, want)
			}
		})
	}
}

func liveStatus(t *testing.T, address string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/livez", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: requestWindow}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestUnexpectedExitRecoveryWaitsForInheritedLock(t *testing.T) {
	c := config(t)
	c.Stdout, c.Stderr = nil, nil
	exitFile := filepath.Join(c.Root, "exit")
	ready := filepath.Join(c.Root, "ready")
	c.Command = []string{"/bin/sh", "-c", `sleep 600 & echo ready > "$1"; while [ ! -f "$2" ]; do sleep 0.05; done; exit 3`, "child", ready, exitFile}
	cancel := startSupervisor(t, c)
	first := awaitPhase(t, c.Address, c.Key, "Running")
	t.Cleanup(func() { _ = syscall.Kill(-first.PID, syscall.SIGKILL) })
	awaitChildReady(t, ready)
	if err := os.WriteFile(exitFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, c.Address, c.Key, "ExitedUnrequested")
	cancel()
	nextConfig := c
	nextConfig.Address = testAddress(t)
	nextConfig.Command = idleCommand()
	startSupervisor(t, nextConfig)
	next := awaitPhase(t, nextConfig.Address, c.Key, "WaitingForExclusiveVolume")
	if next.PID != 0 || liveStatus(t, nextConfig.Address) != http.StatusOK {
		t.Fatal("successor bypassed inherited lock or failed liveness while waiting")
	}
	if f, err := openLock(c.Root, false); err == nil {
		_ = f.Close()
		t.Fatal("unexpected exit released a surviving descendant's lock")
	}
	_ = syscall.Kill(-first.PID, syscall.SIGKILL)
	next = awaitPhase(t, nextConfig.Address, c.Key, "Running")
	if next.Generation == first.Generation || next.Invocation == first.Invocation || next.DiskID != first.DiskID {
		t.Fatal("recovery reused failed invocation or changed retained disk")
	}
}

func TestLivenessRecoveryOnSamePodAndDisk(t *testing.T) {
	for _, exitCode := range []string{"0", "3", "7"} {
		t.Run("exit-"+exitCode, func(t *testing.T) {
			c := config(t)
			exitFile := filepath.Join(c.Root, "exit")
			c.Command = []string{"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.05; done; exit "$2"`, "child", exitFile, exitCode}
			cancel := startSupervisor(t, c)
			first := awaitPhase(t, c.Address, c.Key, "Running")
			// This fixture has no application listener: readiness can fail while
			// the launcher remains live, including during runtime recovery.
			if liveStatus(t, c.Address) != http.StatusOK {
				t.Fatal("live child reported unhealthy")
			}
			if err := os.WriteFile(exitFile, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			failed := awaitPhase(t, c.Address, c.Key, "ExitedUnrequested")
			if liveStatus(t, c.Address) != http.StatusServiceUnavailable {
				t.Fatal("unexpected exit did not fail liveness")
			}
			if !failed.ChildExited || failed.RemovalReady() || failed.RuntimeDataSafe() || failed.RestartDenied || failed.InheritedLockReleased {
				t.Fatalf("unexpected exit produced proof: %+v", failed)
			}
			// Model kubelet's SIGTERM after failed liveness, then restart the
			// container with the same Pod UID, host, boot and retained volume.
			cancel()
			nextConfig := c
			nextConfig.Address = testAddress(t)
			nextConfig.Command = idleCommand()
			startSupervisor(t, nextConfig)
			next := awaitPhase(t, nextConfig.Address, c.Key, "Running")
			if next.PodUID != first.PodUID || next.DiskID != first.DiskID || next.BootID != first.BootID || next.Generation == first.Generation || next.Invocation == first.Invocation {
				t.Fatalf("invalid recovery identity: before=%+v after=%+v", first, next)
			}
			if liveStatus(t, nextConfig.Address) != http.StatusOK {
				t.Fatal("recovered launcher is unhealthy")
			}
			if _, err := query(t, nextConfig.Address, c.Key, "stale", first.Generation); err == nil {
				t.Fatal("recovered child accepted fenced generation")
			}
			if _, err := os.Stat(retiredDiskPath(c.Root)); !os.IsNotExist(err) {
				t.Fatalf("unexpected exit retired storage: %v", err)
			}
		})
	}
}
