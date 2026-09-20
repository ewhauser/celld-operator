package launcher

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHandoffBoundToExactInvocation(t *testing.T) {
	state := State{Phase: "WaitingForHandoff", Invocation: "inv", Generation: "gen", PodUID: "pod", Host: "new", BootID: "boot", DiskID: "disk", PreviousHost: "old\noldboot"}
	correct := Handoff{Invocation: state.Invocation, Generation: state.Generation, PodUID: state.PodUID, Host: state.Host, BootID: state.BootID, DiskID: state.DiskID, PreviousHost: state.PreviousHost}
	for _, field := range []string{"valid", "invocation", "generation", "pod", "host", "boot", "disk", "previous", "expired", "skew", "future", "stop"} {
		t.Run(field, func(t *testing.T) {
			s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: state, handoff: make(chan struct{})}
			h := correct
			q := Request{Nonce: Nonce(), NotAfterMS: time.Now().Add(time.Second).UnixMilli(), Handoff: &h}
			switch field {
			case "invocation":
				h.Invocation = "other"
			case "generation":
				h.Generation = "other"
			case "pod":
				h.PodUID = "other"
			case "host":
				h.Host = "other"
			case "boot":
				h.BootID = "other"
			case "disk":
				h.DiskID = "other"
			case "previous":
				h.PreviousHost = "other"
			case "expired":
				q.NotAfterMS = 0
			case "skew":
				// The controller stamps now+3s on its own clock; a few seconds
				// of forward skew must still land inside requestExpiryBound.
				q.NotAfterMS = time.Now().Add(5 * time.Second).UnixMilli()
			case "future":
				q.NotAfterMS = time.Now().Add(requestExpiryBound + time.Second).UnixMilli()
			case "stop":
				q.Operation = "stop"
			}
			b, _ := json.Marshal(q)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
			req.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if (rec.Code == 200) != (field == "valid" || field == "skew") {
				t.Fatalf("code %d", rec.Code)
			}
			if field == "valid" {
				s.state.Invocation = "successor"
				rec = httptest.NewRecorder()
				req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
				req.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
				s.ServeHTTP(rec, req)
				if rec.Code == 200 {
					t.Fatal("replayed grant admitted successor")
				}
			}
		})
	}
}

func TestHandoffGrantIsIdempotent(t *testing.T) {
	state := State{Phase: "WaitingForHandoff", Invocation: "inv", Generation: "gen", PodUID: "pod", Host: "new", BootID: "boot", DiskID: "disk", PreviousHost: "old\noldboot"}
	s := &supervisor{key: bytes.Repeat([]byte{1}, 32), state: state, handoff: make(chan struct{})}
	q := Request{Nonce: Nonce(), NotAfterMS: time.Now().Add(time.Second).UnixMilli(), Handoff: &Handoff{Invocation: state.Invocation, Generation: state.Generation, PodUID: state.PodUID, Host: state.Host, BootID: state.BootID, DiskID: state.DiskID, PreviousHost: state.PreviousHost}}
	b, _ := json.Marshal(q)
	// A retried grant for the same association must answer 200 again without
	// closing the handoff channel twice.
	for i := range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1", bytes.NewReader(b))
		req.Header.Set("X-Celld-MAC", MAC(s.key, "request", q))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("grant %d: code %d", i, rec.Code)
		}
	}
	select {
	case <-s.handoff:
	default:
		t.Fatal("handoff not released")
	}
}

func TestCrossHostWaitsForLiveGrant(t *testing.T) {
	c := config(t)
	if err := os.WriteFile(c.Root+"/.celld-launcher-host", []byte("old\noldboot"), 0o600); err != nil {
		t.Fatal(err)
	}
	disk := strings.Repeat("d", 64)
	if err := os.WriteFile(c.Root+"/.celld-launcher-disk", []byte(disk), 0o600); err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, c)
	state := awaitPhase(t, c.Address, c.Key, "WaitingForHandoff")
	if state.PID != 0 || state.DiskID != disk || state.BootID != c.BootID {
		t.Fatalf("unexpected state %+v", state)
	}
	q := Request{Nonce: Nonce(), NotAfterMS: time.Now().Add(time.Second).UnixMilli(), Handoff: &Handoff{Invocation: state.Invocation, Generation: state.Generation, PodUID: state.PodUID, Host: state.Host, BootID: state.BootID, DiskID: state.DiskID, PreviousHost: state.PreviousHost}}
	b, _ := json.Marshal(q)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+c.Address+"/v1", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Celld-MAC", MAC(c.Key, "request", q))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	awaitPhase(t, c.Address, c.Key, "Running")
	stamp, err := os.ReadFile(c.Root + "/.celld-launcher-host")
	if err != nil || string(stamp) != c.Host+"\n"+c.BootID {
		t.Fatalf("stamp %s %v", stamp, err)
	}
}

func TestLegacyStateMACRemainsReadable(t *testing.T) {
	// Optional transfer fields must not change the canonical JSON of an old
	// launcher's response. A v6 launcher remains readable, not transferable.
	old := struct {
		PodUID, Node, Host, Invocation, Generation, Phase, Operation, Error string
		PID                                                                 int
	}{PodUID: "pod", Node: "node", Host: "host", Invocation: "inv", Generation: "gen", Phase: "Running", PID: 42}
	body, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var current State
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatal(err)
	}
	key := []byte("test-key")
	if !Verify(key, "response", current, MAC(key, "response", old)) {
		t.Fatal("legacy launcher signature changed")
	}
}

func TestRetiredPodCannotRestartButNewPodCanReuseDisk(t *testing.T) {
	c := config(t)
	cancel := startSupervisor(t, c)
	first := awaitPhase(t, c.Address, c.Key, "Running")
	if _, err := query(t, c.Address, c.Key, "retirement", first.Generation); err != nil {
		t.Fatal(err)
	}
	receipt := awaitPhase(t, c.Address, c.Key, "Stopped")
	if !receipt.RestartDenied {
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
	if state.PID != 0 || state.Operation != "" {
		t.Fatal("disk marker reconstructed positive stopped authority or started child")
	}
	stopStale()
	next := c
	next.Address = testAddress(t)
	next.PodUID = "new-pod"
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
	if state.PID != 0 || state.Operation != "" {
		t.Fatal("marker manufactured a stopped receipt")
	}
}
