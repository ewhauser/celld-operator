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
	"runtime"
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
func query(t *testing.T, address string, key []byte, op, gen string) (State, error) {
	t.Helper()
	q := Request{Nonce: Nonce(), Operation: op, Generation: gen, NotAfterMS: time.Now().Add(time.Second).UnixMilli()}
	b, _ := json.Marshal(q)
	req, e := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/v1", bytes.NewReader(b))
	if e != nil {
		return State{}, e
	}
	req.Header.Set("X-Celld-MAC", MAC(key, "request", q))
	c := &http.Client{Timeout: time.Second}
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
func awaitPhase(t *testing.T, address string, key []byte, phase string) State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
func config(t *testing.T) Config {
	return Config{Root: t.TempDir(), Address: testAddress(t), Key: bytes.Repeat([]byte{7}, 32), PodUID: "pod", Node: "node", Host: "host", BootID: "test-boot", Command: []string{"/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}, Stdout: io.Discard, Stderr: io.Discard}
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
	b, _ := json.Marshal(c)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLauncherHelper$")
	cmd.Env = append(os.Environ(), "LAUNCHER_HELPER=1", "LAUNCHER_CONFIG="+string(b))
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	first := awaitPhase(t, c.Address, c.Key, "Running")
	if e := syscall.Kill(-first.PID, syscall.SIGSTOP); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = syscall.Kill(-first.PID, syscall.SIGKILL) })
	// On Linux Pdeathsig kills the direct child. Its sleep descendant still owns
	// the inherited descriptor, so no successor may use the volume prematurely.
	time.Sleep(100 * time.Millisecond)
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	// A surviving descendant is platform-dependent; explicitly confirm the
	// lock owner survived rather than treating process absence as proof.
	f, e := openLock(c.Root, true)
	if e == nil {
		_ = f.Close()
		t.Skip("no descendant survived direct-child termination")
	}
	replacement := c
	replacement.Address = testAddress(t)
	startSupervisor(t, replacement)
	awaitPhase(t, replacement.Address, c.Key, "WaitingForExclusiveVolume")
	_ = syscall.Kill(-first.PID, syscall.SIGKILL)
	awaitPhase(t, replacement.Address, c.Key, "Running")
}
func TestRequestMACDomainSeparated(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	q := Request{Nonce: Nonce()}
	if Verify(key, "response", q, MAC(key, "request", q)) {
		t.Fatal("request accepted as response")
	}
}
