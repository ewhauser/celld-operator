package launcher

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stopRequest(t *testing.T, s *supervisor, op string, notAfter time.Time) (int, State) {
	t.Helper()
	q := Request{Nonce: Nonce(), Operation: op, Generation: "g", NotAfterMS: notAfter.UnixMilli(), DeadlineMS: time.Now().Add(time.Minute).UnixMilli()}
	b, _ := json.Marshal(q)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v2", bytes.NewReader(b))
	req.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var ans Response
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&ans); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, ans.State
}

func TestStopRequestExpiryIsEnforcedInEveryPhase(t *testing.T) {
	for _, phase := range []string{"Running", "Draining", "Stopped"} {
		t.Run(phase, func(t *testing.T) {
			s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: State{Phase: phase, Generation: "g", Operation: "op"}, stop: make(chan struct{}), stopping: phase != "Running"}
			if code, _ := stopRequest(t, s, "op", time.Now().Add(-time.Second)); code != http.StatusConflict {
				t.Fatalf("expired request accepted in %s: %d", phase, code)
			}
			if code, _ := stopRequest(t, s, "op", time.Now().Add(time.Hour)); code != http.StatusConflict {
				t.Fatalf("far-future expiry accepted in %s: %d", phase, code)
			}
			if code, st := stopRequest(t, s, "op", time.Now().Add(time.Second)); code != http.StatusOK || st.Operation != "op" {
				t.Fatalf("valid retry of the accepted operation refused in %s: %d %+v", phase, code, st)
			}
		})
	}
}

func TestOperationBindsOnlyToRunningChild(t *testing.T) {
	for _, phase := range []string{"Terminating", "Draining", "Stopped", "ReleasingInheritedLock", "ExitedUnrequested", "Blocked"} {
		t.Run(phase, func(t *testing.T) {
			// No operation was accepted while Running; this exit belongs to nobody.
			s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: State{Phase: phase, Generation: "g"}, stop: make(chan struct{}), stopping: true}
			code, _ := stopRequest(t, s, "late", time.Now().Add(time.Second))
			if code != http.StatusConflict {
				t.Fatalf("a %s supervisor adopted a later operation: %d", phase, code)
			}
			if s.state.Operation != "" || s.state.Phase != phase {
				t.Fatalf("state mutated by refused binding: %+v", s.state)
			}
		})
	}
	s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: State{Phase: "Stopped", Generation: "g", Operation: "first"}, stop: make(chan struct{}), stopping: true}
	if code, _ := stopRequest(t, s, "second", time.Now().Add(time.Second)); code != http.StatusConflict {
		t.Fatal("stopped supervisor answered a different operation")
	}
}

func TestUnrequestedTerminationNeverCertifiesAnOperation(t *testing.T) {
	c := config(t)
	cancel := startSupervisor(t, c)
	first := awaitPhase(t, c.Address, c.Key, "Running")
	cancel() // kubelet SIGTERM, not a controller stop
	deadline := time.Now().Add(10 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		st, err := query(t, c.Address, c.Key, "", "")
		if err != nil {
			break // listener closed with the supervisor
		}
		last = st
		if st.Phase == "Terminating" || st.Phase == "ReleasingInheritedLock" || st.Phase == "Stopped" {
			if _, err := query(t, c.Address, c.Key, "adopt", first.Generation); err == nil {
				t.Fatalf("operation bound during unrequested termination in phase %s", st.Phase)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.Operation != "" {
		t.Fatalf("unrequested exit carried an operation: %+v", last)
	}
}

func TestRequestedStopWaitsForInheritedLockRelease(t *testing.T) {
	c := config(t)
	// A descendant keeps FD3 for three seconds after the child exits; the old
	// fixed two-second window would have blocked forever instead of certifying.
	// Real file descriptors avoid os/exec copy goroutines delaying Wait until
	// descendants close stdout/stderr, hiding the inherited-lock branch.
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	c.Stdout, c.Stderr = out, out
	ready := filepath.Join(c.Root, "ready")
	c.Command = []string{"/bin/sh", "-c", `trap 'exit 0' TERM; (sleep 3) & touch "$1"; while :; do sleep 1; done`, "child", ready}
	startSupervisor(t, c)
	awaitChildReady(t, ready)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	if _, err := query(t, c.Address, c.Key, "retire", running.Generation); err != nil {
		t.Fatal(err)
	}
	sawWait := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st, err := query(t, c.Address, c.Key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if st.Phase == "ReleasingInheritedLock" {
			sawWait = true
		}
		if st.Phase == "Stopped" {
			if !sawWait {
				t.Fatal("never exercised ReleasingInheritedLock")
			}
			if st.Operation != "retire" || !st.RestartDenied {
				t.Fatalf("incomplete certificate %+v", st)
			}
			return
		}
		if st.Phase == "Blocked" {
			t.Fatalf("blocked instead of waiting for the descendant: %s", st.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("stop never certified")
}

func TestReplacedLockFileNeverCertifies(t *testing.T) {
	c := config(t)
	startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	// An administrator or runtime sweep removes the lock file while the child
	// holds the original inode. A successor could now lock a different file.
	if err := os.Remove(filepath.Join(c.Root, ".celld-launcher.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := query(t, c.Address, c.Key, "retire", running.Generation); err != nil {
		t.Fatal(err)
	}
	st := awaitPhase(t, c.Address, c.Key, "Blocked")
	if !strings.Contains(st.Error, "replaced") || st.RestartDenied {
		t.Fatalf("expected replaced-lock block without a certificate: %+v", st)
	}
}

func TestEmptyHostIdentityFileBlocksInsteadOfMisreportingHost(t *testing.T) {
	c := config(t)
	if err := os.WriteFile(filepath.Join(c.Root, ".celld-launcher-host"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, c)
	st := awaitPhase(t, c.Address, c.Key, "Blocked")
	if !strings.Contains(st.Error, "corrupt") || st.PID != 0 {
		t.Fatalf("corrupt identity file misreported: %+v", st)
	}
}

func TestPersistHostIsExclusiveAndLeavesNoTemporaries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".celld-launcher-host")
	if err := persistHost(root, path, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := persistHost(root, path, []byte("b")); err == nil {
		t.Fatal("second exclusive create succeeded")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a" {
		t.Fatalf("content replaced: %q", got)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestStopGraceEscalatesToKillOnSchedule(t *testing.T) {
	if os.Getenv("LAUNCHER_TESTS_EMULATED") != "" {
		// Under user-mode QEMU the SIGTERM reaches the emulator process, whose
		// default disposition exits, so a child cannot ignore it; the fixture, not
		// the escalation, is what emulation breaks. Native runs cover this test.
		t.Skip("SIGTERM-ignoring fixture is not reproducible under CPU emulation")
	}
	c := config(t)
	// The child ignores SIGTERM; only the configured escalation ends it. Running
	// means the shell has been started, not that it has reached its first
	// statement, so wait for the fixture to announce that the trap is installed:
	// a SIGTERM that arrives before it is still fatal by default disposition and
	// would end the child in milliseconds rather than at the escalation.
	ready, command := shellFixture(t)
	c.Command = command("trap '' TERM")
	c.StopGrace = 2 * time.Second
	startSupervisor(t, c)
	awaitChildReady(t, ready)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	awaitReady(t, ready)
	started := time.Now()
	if _, err := query(t, c.Address, c.Key, "retire", running.Generation); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := query(t, c.Address, c.Key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if st.Phase == "Stopped" {
			if elapsed := time.Since(started); elapsed < 2*time.Second || elapsed > 6*time.Second {
				t.Fatalf("escalation did not follow the configured grace: %v", elapsed)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("child ignoring SIGTERM was never killed")
}

func TestChildExitWithoutRequestRetainsLockAndRejectsAdoption(t *testing.T) {
	c := config(t)
	// File-controlled exit ensures the test observes Running before the child exits.
	exitFile := filepath.Join(c.Root, "exit")
	c.Command = []string{"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.05; done; exit 7`, "child", exitFile}
	cancel := startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	if err := os.WriteFile(exitFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := awaitPhase(t, c.Address, c.Key, "ExitedUnrequested")
	if st.Operation != "" || st.RestartDenied || st.Generation != running.Generation || !strings.Contains(st.Error, "exit status 7") {
		t.Fatalf("unsolicited exit certified or lost identity: %+v", st)
	}
	if _, err := query(t, c.Address, c.Key, "late", running.Generation); err == nil {
		t.Fatal("late operation adopted exit")
	}
	replacement := c
	replacement.Address = testAddress(t)
	startSupervisor(t, replacement)
	awaitPhase(t, replacement.Address, c.Key, "WaitingForExclusiveVolume")
	// Releasing the failed supervisor is the only way its successor can launch.
	cancel()
	next := awaitPhase(t, replacement.Address, c.Key, "ExitedUnrequested")
	if next.Generation == running.Generation {
		t.Fatal("successor reused failed invocation")
	}
}

func awaitChildReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not install its signal handler")
}
