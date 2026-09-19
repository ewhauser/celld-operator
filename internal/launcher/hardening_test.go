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
	q := Request{Nonce: Nonce(), Operation: op, Generation: "g", NotAfterMS: notAfter.UnixMilli()}
	b, _ := json.Marshal(q)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
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
	for _, phase := range []string{"Running", "Stopping", "Stopped"} {
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
	for _, phase := range []string{"Terminating", "Stopping", "Stopped", "ReleasingInheritedLock", "ExitedUnrequested", "Blocked"} {
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
	c.Command = []string{"/bin/sh", "-c", "trap 'exit 0' TERM; (sleep 3) & while :; do sleep 1; done"}
	startSupervisor(t, c)
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
				t.Log("lock released before the wait was observed; certification still correct")
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
	// The child ignores SIGTERM; only the configured escalation ends it.
	c.Command = []string{"/bin/sh", "-c", "trap '' TERM; while :; do sleep 1; done"}
	c.StopGrace = 2 * time.Second
	startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
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
