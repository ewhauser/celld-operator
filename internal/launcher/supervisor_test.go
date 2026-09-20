package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testAddress(t *testing.T) string {
	t.Helper()
	l, e := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

// requestWindow is the expiry a test request carries and the client timeout it
// waits with. It stays below requestExpiryBound so the supervisor accepts it,
// but is wide enough that a scheduling delay under -race on a loaded runner
// cannot expire a request the test means to succeed.
const requestWindow = 5 * time.Second

func query(t *testing.T, address string, key []byte, op, gen string) (State, error) {
	t.Helper()
	q := Request{Nonce: Nonce(), Operation: op, Generation: gen, NotAfterMS: time.Now().Add(requestWindow).UnixMilli()}
	b, _ := json.Marshal(q)
	req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/v1", bytes.NewReader(b))
	if e != nil {
		return State{}, e
	}
	req.Header.Set("X-Celld-MAC", MAC(key, "request", q))
	c := &http.Client{Timeout: requestWindow}
	resp, e := c.Do(req)
	if e != nil {
		return State{}, e
	}
	defer func() { _ = resp.Body.Close() }()
	var ans Response
	e = json.NewDecoder(resp.Body).Decode(&ans)
	if e != nil {
		return State{}, e
	}
	signed := struct {
		Nonce string
		State State
	}{ans.Nonce, ans.State}
	if ans.Nonce != q.Nonce || !Verify(key, "response", signed, ans.MAC) {
		t.Fatal("unauthenticated response")
	}
	return ans.State, nil
}

// shellFixture builds a /bin/sh child that runs prologue, then announces its
// readiness by writing the PID of its last background job (empty when it has
// none) to the returned path, and finally idles.
//
// Signaling a shell that has only just been started is a race: until sh has
// executed the prologue, the disposition a test relies on is not installed yet
// and the default one applies. Tests therefore wait for the readiness file
// before asking the supervisor to stop the child.
func shellFixture(t *testing.T) (ready string, command func(prologue string) []string) {
	t.Helper()
	ready = filepath.Join(t.TempDir(), "fixture-ready")
	return ready, func(prologue string) []string {
		// The trailing newline marks the write complete, so a reader never sees a
		// half-written PID. Statements are newline-separated: a prologue may end in
		// "&", which no ";" may follow.
		announce := "printf '%s\\n' \"$!\" > '" + ready + "'"
		return []string{"/bin/sh", "-c", prologue + "\n" + announce + "\nwhile :; do sleep 1; done\n"}
	}
}

// awaitReady blocks until a shellFixture child has announced itself and returns
// what it announced: the PID of its background job, or "" when it has none.
func awaitReady(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && bytes.HasSuffix(b, []byte("\n")) {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture never announced readiness at %s", path)
	return ""
}

func awaitPhase(t *testing.T, address string, key []byte, phase string) State {
	t.Helper()
	// Only the failure path waits this long: the poll returns as soon as the
	// phase appears. A short deadline turned ordinary CI scheduling delay into a
	// test failure, so give a phase that genuinely never arrives time to prove it.
	deadline := time.Now().Add(30 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		s, e := query(t, address, key, "", "")
		if e == nil && s.Phase == phase {
			return s
		}
		if e == nil {
			last = s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("phase %s not reached; last observed %+v", phase, last)
	return State{}
}

// idleCommand is the ordinary fixture child: it idles until asked to stop and
// leaves behind only sleeps that release the inherited descriptor within a
// second, so a supervisor running it shuts down promptly.
func idleCommand() []string {
	return []string{"/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}
}

func config(t *testing.T) Config {
	return Config{Root: t.TempDir(), Address: testAddress(t), Key: bytes.Repeat([]byte{7}, 32), PodUID: "pod", Node: "node", Host: "host", BootID: "test-boot", Command: idleCommand(), Stdout: io.Discard, Stderr: io.Discard}
}
func startSupervisor(t *testing.T, c Config) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = Run(ctx, c); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("supervisor cleanup timed out")
		}
	})
	return cancel
}
func TestPausedOwnerPreventsReplacement(t *testing.T) {
	c := config(t)
	cancel := startSupervisor(t, c)
	first := awaitPhase(t, c.Address, c.Key, "Running")
	replacement := c
	replacement.Address = testAddress(t)
	replacement.PodUID = "replacement"
	startSupervisor(t, replacement)
	waiting := awaitPhase(t, replacement.Address, c.Key, "WaitingForExclusiveVolume")
	if waiting.PID != 0 {
		t.Fatal("replacement child launched before lock")
	}
	stopped, e := query(t, c.Address, c.Key, "retire", first.Generation)
	if e != nil || stopped.Operation != "retire" {
		t.Fatalf("stop: %+v %v", stopped, e)
	}
	awaitPhase(t, c.Address, c.Key, "Stopped")
	awaitPhase(t, replacement.Address, c.Key, "WaitingForExclusiveVolume")
	cancel()
	next := awaitPhase(t, replacement.Address, c.Key, "Running")
	if next.Generation == first.Generation {
		t.Fatal("generation reused")
	}
}
func TestProtocolRejectsForgedAndRetargetedStop(t *testing.T) {
	s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: State{Phase: "Running", Generation: "generation"}, stop: make(chan struct{})}
	q := Request{Nonce: Nonce(), Operation: "one", Generation: "wrong", NotAfterMS: time.Now().Add(time.Second).UnixMilli()}
	for _, key := range [][]byte{bytes.Repeat([]byte{2}, 32), s.key} {
		b, _ := json.Marshal(q)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
		req.Header.Set("X-Celld-MAC", MAC(key, "request", q))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Fatal("unauthorized stop accepted")
		}
	}
	if s.stopping {
		t.Fatal("invalid request stopped child")
	}
	q.Generation = "generation"
	for _, op := range []string{"one", "one", "two"} {
		q.Operation = op
		b, _ := json.Marshal(q)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
		req.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if (rec.Code == 200) != (op == "one") {
			t.Fatal("operation replay authority changed")
		}
	}
}
func TestCrossHostAndCleanMarkerBlock(t *testing.T) {
	for _, which := range []string{"host", "marker"} {
		t.Run(which, func(t *testing.T) {
			c := config(t)
			name, value := ".celld-launcher-host", "other"
			if which == "marker" {
				name, value = ".clean-reload.json", "{}"
			}
			if e := os.WriteFile(c.Root+"/"+name, []byte(value), 0o600); e != nil {
				t.Fatal(e)
			}
			startSupervisor(t, c)
			state := awaitPhase(t, c.Address, c.Key, "Blocked")
			if state.PID != 0 {
				t.Fatal("blocked startup spawned child")
			}
		})
	}
}
func TestLauncherHelper(t *testing.T) {
	if os.Getenv("LAUNCHER_HELPER") != "1" {
		return
	}
	var c Config
	if e := json.Unmarshal([]byte(os.Getenv("LAUNCHER_CONFIG")), &c); e != nil {
		os.Exit(4)
	}
	_ = Run(context.Background(), c)
	os.Exit(0)
}
func TestKilledLauncherCannotUnlockSurvivingChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux production descriptor and parent-death semantics; run cross-compiled suite in pinned image")
	}
	c := config(t)
	c.Stdout = nil
	c.Stderr = nil
	// The descendant that must outlive the launcher is an explicit long sleep, not
	// whichever one-second sleep the idle loop happened to be running: that one
	// releases the inherited descriptor within a second, after which a successor
	// may legitimately acquire the volume and the test's premise disappears.
	ready, command := shellFixture(t)
	c.Command = command("sleep 600 &")
	b, _ := json.Marshal(c)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLauncherHelper$")
	cmd.Env = append(os.Environ(), "LAUNCHER_HELPER=1", "LAUNCHER_CONFIG="+string(b))
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	first := awaitPhase(t, c.Address, c.Key, "Running")
	t.Cleanup(func() { _ = syscall.Kill(-first.PID, syscall.SIGKILL) })
	descendant, e := strconv.Atoi(awaitReady(t, ready))
	if e != nil {
		t.Fatalf("fixture announced no descendant: %v", e)
	}
	// On Linux Pdeathsig kills the direct child. Its sleep descendant still owns
	// the inherited descriptor, so no successor may use the volume prematurely.
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	// The direct child may still be dying, and a dying process still holds FD3, so
	// a lock probe taken now cannot tell a survivor from the child's last moment.
	// Wait for the child to release its descriptors before drawing any conclusion.
	awaitReleased(t, first.PID)
	// A surviving descendant is platform-dependent; explicitly confirm the
	// lock owner survived rather than treating process absence as proof.
	if !alive(descendant) {
		t.Skip("no descendant survived direct-child termination")
	}
	if f, e := openLock(c.Root, true); e == nil {
		_ = f.Close()
		t.Fatalf("lock acquired while descendant %d still holds the inherited descriptor", descendant)
	}
	replacement := c
	replacement.Address = testAddress(t)
	// The successor only has to reach Running; give it the ordinary child so its
	// own shutdown is not held up by a long-lived descendant of its own.
	replacement.Command = idleCommand()
	startSupervisor(t, replacement)
	awaitPhase(t, replacement.Address, c.Key, "WaitingForExclusiveVolume")
	_ = syscall.Kill(-first.PID, syscall.SIGKILL)
	awaitPhase(t, replacement.Address, c.Key, "Running")
}

// alive reports whether pid names a process that still holds its descriptors: a
// zombie has released them and is therefore not alive for this test's purpose.
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) state ..."; comm may contain spaces or parentheses.
	fields := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

// awaitReleased blocks until pid has released its descriptors.
func awaitReleased(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child %d never died after the launcher was killed", pid)
}
func TestRequestMACDomainSeparated(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	q := Request{Nonce: Nonce()}
	if Verify(key, "response", q, MAC(key, "request", q)) {
		t.Fatal("request accepted as response")
	}
}

// The two waits an unrequested termination spends must fit inside the pod's
// grace period, leaving the margin the rest of the shutdown needs.
func TestTerminationBudgetFitsTheGracePeriod(t *testing.T) {
	for _, grace := range []time.Duration{0, 6 * time.Second, 30 * time.Second, 180 * time.Second, 3605 * time.Second} {
		stop, proof := terminationBudget(grace)
		want := grace
		if want == 0 {
			want = defaultGrace
		}
		if stop <= 0 || proof <= 0 {
			t.Fatalf("grace %v: unusable bounds stop=%v proof=%v", grace, stop, proof)
		}
		if proof > maxLockProof {
			t.Fatalf("grace %v: lock proof %v exceeds the cap", grace, proof)
		}
		if stop+proof+graceMargin > want {
			t.Fatalf("grace %v: %v+%v+%v exceeds the grace period", grace, stop, proof, graceMargin)
		}
	}
	if stop, proof := terminationBudget(30 * time.Second); stop != 15*time.Second || proof != 10*time.Second {
		t.Fatalf("default grace splits as %v+%v, want 15s+10s", stop, proof)
	}
}
