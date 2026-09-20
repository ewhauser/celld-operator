package launcher

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
)

// The runtime fixture serves the real strict wire contract through HTTP and the
// production typed client. Terminal /state has no actor or load object.
type strictRuntime struct {
	mu                      sync.Mutex
	client                  *controlplane.Client
	operation, phase, fault string
	posts, polls            int
}

func newStrictRuntime(t *testing.T, c Config) *strictRuntime {
	t.Helper()
	f := &strictRuntime{phase: "data_safe"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, err := sendRequestContext(t, r.Context(), c.Address, c.Key, Request{Nonce: Nonce()})
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/shutdown":
			if r.Method != "POST" || r.URL.RawQuery != "mode=remove-disk" {
				http.Error(w, "ordinary shutdown forbidden", http.StatusBadRequest)
				return
			}
			var body struct {
				Operation  string `json:"operation_id"`
				Generation string `json:"expected_generation"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.Generation != state.Generation {
				http.Error(w, "wrong generation", http.StatusConflict)
				return
			}
			f.operation = body.Operation
			f.posts++
		case "/state":
			if f.operation != "" {
				f.polls++
			}
		default:
			http.NotFound(w, r)
			return
		}
		if f.fault == "outage" || (f.fault == "lost-result" && f.operation != "" && r.Method == "GET") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		generation, operation, mode := state.Generation, f.operation, "remove-disk"
		if r.Method == "GET" && f.operation != "" {
			switch f.fault {
			case "generation":
				generation = "other"
			case "operation":
				operation = "other"
			case "mode":
				mode = "preserve"
			}
		}
		var op any
		phase, controlOnly := f.phase, f.phase != "draining"
		if r.Method == "POST" && f.fault != "lost-result" {
			phase, controlOnly = "draining", false
		}
		if operation != "" {
			var blocker any
			if phase == "failed" {
				blocker = "recovery unavailable"
			}
			op = map[string]any{"operation_id": operation, "expected_generation": generation, "mode": mode, "phase": phase, "blocker": blocker}
		}
		shutdown := map[string]any{"schema_version": 1, "runtime_generation": generation, "capabilities": map[string]bool{"strict_disk_removal": f.fault != "unsupported"}, "control_only": controlOnly, "operation": op}
		if r.Method == "POST" {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(shutdown)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"shutdown": shutdown})
		}
	}))
	t.Cleanup(server.Close)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	f.client = controlplane.New(transport)
	return f
}

func (f *strictRuntime) update(phase, fault string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase, f.fault = phase, fault
}

func (f *strictRuntime) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.polls
}

func TestStopWithoutStrictCapabilityCannotCertify(t *testing.T) {
	c := config(t)
	f := newStrictRuntime(t, c)
	f.update("draining", "unsupported")
	c.Control = f.client
	startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	if _, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil {
		t.Fatal(err)
	}
	state := awaitPhase(t, c.Address, c.Key, "Failed")
	if state.RuntimeDataSafe() || state.RemovalReady() || state.ChildExited {
		t.Fatalf("unsupported child certified: %+v", state)
	}
	if posts, _ := f.counts(); posts != 0 {
		t.Fatal("unsupported runtime received shutdown mutation")
	}
}

func TestAcceptanceDoesNotTerminateAndControlOnlyCompletionDoes(t *testing.T) {
	c := config(t)
	f := newStrictRuntime(t, c)
	f.update("draining", "")
	c.Control = f.client
	ready, command := shellFixture(t)
	c.Command = command("trap 'exit 0' TERM")
	startSupervisor(t, c)
	awaitReady(t, ready)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	for range 2 {
		if _, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := query(t, c.Address, c.Key, "conflict", running.Generation); err == nil {
		t.Fatal("conflicting operation accepted")
	}
	// Poll for enough time that the old SIGTERM path would have exited.
	until := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(until) {
		state, err := query(t, c.Address, c.Key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase != "Draining" || state.ChildExited || state.RuntimeDataSafe() {
			t.Fatalf("202 treated as completion: %+v", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.update("data_safe", "")
	stopped := awaitPhase(t, c.Address, c.Key, "Stopped")
	if !stopped.RemovalReady() {
		t.Fatalf("incomplete proof: %+v", stopped)
	}
	if posts, polls := f.counts(); posts != 1 || polls < 2 {
		t.Fatalf("duplicate side effects or missing status reads: %d/%d", posts, polls)
	}
	before := stopped
	if stopped, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil || stopped != before {
		t.Fatalf("duplicate changed immutable result: %+v %v", stopped, err)
	}
}

func TestStrictFailureOrLostResultCannotBecomeSuccessAfterExit(t *testing.T) {
	for _, fault := range []string{"failed", "generation", "operation", "mode", "outage", "lost-result"} {
		t.Run(fault, func(t *testing.T) {
			c := config(t)
			f := newStrictRuntime(t, c)
			if fault == "failed" {
				f.update("failed", "")
			} else {
				f.update("data_safe", fault)
			}
			c.Control = f.client
			exitFile := filepath.Join(c.Root, "exit")
			c.Command = []string{"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.05; done; exit 0`, "child", exitFile}
			startSupervisor(t, c)
			running := awaitPhase(t, c.Address, c.Key, "Running")
			if _, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil {
				t.Fatal(err)
			}
			failed := awaitPhase(t, c.Address, c.Key, "Failed")
			if failed.RuntimeDataSafe() || failed.RemovalReady() || failed.ChildExited {
				t.Fatalf("invalid proof: %+v", failed)
			}
			if err := os.WriteFile(exitFile, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(5 * time.Second)
			for time.Now().Before(until) {
				state, err := query(t, c.Address, c.Key, "", "")
				if err != nil {
					t.Fatal(err)
				}
				if state.Phase == "Stopped" || state.RuntimeDataSafe() || state.RemovalReady() {
					t.Fatalf("failure became success after exit: %+v", state)
				}
				if state.RestartDenied && state.ChildExited {
					if state.Error != failed.Error || state.Removal != failed.Removal {
						t.Fatal("lost terminal failure")
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("failed child exit was not captured")
		})
	}
}

func TestRemovalDeadlineCannotBeExtendedOrManufactureDataSafety(t *testing.T) {
	c := config(t)
	f := newStrictRuntime(t, c)
	f.update("draining", "")
	c.Control = f.client
	c.StopGrace = 50 * time.Millisecond
	ready, command := shellFixture(t)
	c.Command = command("trap '' TERM")
	startSupervisor(t, c)
	awaitReady(t, ready)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	deadline := time.Now().Add(400 * time.Millisecond).UnixMilli()
	req := Request{Nonce: Nonce(), Operation: "remove", Generation: running.Generation, NotAfterMS: time.Now().Add(time.Second).UnixMilli(), DeadlineMS: deadline}
	if _, err := sendRequest(t, c.Address, c.Key, req); err != nil {
		t.Fatal(err)
	}
	req.DeadlineMS = time.Now().Add(time.Hour).UnixMilli()
	state, err := sendRequest(t, c.Address, c.Key, req)
	if err != nil || state.DeadlineMS != deadline {
		t.Fatalf("retry extended deadline: %+v %v", state, err)
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		state, err = query(t, c.Address, c.Key, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase == "Stopped" || state.RuntimeDataSafe() {
			t.Fatalf("deadline kill certified: %+v", state)
		}
		if state.ChildExited && state.RestartDenied && state.Phase == "Failed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("deadline failed to stop exact child: %+v", state)
}

func TestChildExitDuringDrainLosesRemovalAuthority(t *testing.T) {
	c := config(t)
	f := newStrictRuntime(t, c)
	f.update("draining", "")
	c.Control = f.client
	exitFile := filepath.Join(c.Root, "exit")
	c.Command = []string{"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.05; done; exit 0`, "child", exitFile}
	startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	if _, err := query(t, c.Address, c.Key, "remove", running.Generation); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for {
		if _, polls := f.counts(); polls > 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("never polled strict result")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(exitFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	state := awaitPhase(t, c.Address, c.Key, "Failed")
	if state.RuntimeDataSafe() || state.RemovalReady() {
		t.Fatalf("exit while draining certified: %+v", state)
	}
	// Once result capture failed, even a subsequently reachable status cannot
	// revive it. This supervisor holds only the failure for its bound operation.
	f.update("data_safe", "")
	time.Sleep(150 * time.Millisecond)
	state, err := query(t, c.Address, c.Key, "remove", running.Generation)
	if err != nil || state.RuntimeDataSafe() || state.RemovalReady() {
		t.Fatalf("late result revived exit: %+v %v", state, err)
	}
}

func TestFirstRemovalRequiresBoundedDeadline(t *testing.T) {
	for _, deadline := range []int64{0, time.Now().Add(-time.Second).UnixMilli(), time.Now().Add(25 * time.Hour).UnixMilli()} {
		s := &supervisor{key: []byte("test"), state: State{Phase: "Running", Generation: "g"}, stop: make(chan struct{})}
		q := Request{Nonce: Nonce(), Operation: "op", Generation: "g", NotAfterMS: time.Now().Add(time.Second).UnixMilli(), DeadlineMS: deadline}
		body, _ := json.Marshal(q)
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/v2", strings.NewReader(string(body)))
		request.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusConflict || s.state.Operation != "" {
			t.Fatalf("unbounded operation admitted: %+v", s.state)
		}
	}
}
