package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var target = Target{IP: "127.0.0.1", Node: "node-a", Generation: "generation-a"}

func fixture(phase string) string {
	operation := "null"
	if phase != "" {
		operation = fmt.Sprintf(`{"operation_id":"scale-in-42","expected_generation":"generation-a","mode":"remove-disk","phase":%q,"blocker":null}`, phase)
	}
	return fmt.Sprintf(`{"schema_version":1,"runtime_generation":"generation-a","capabilities":{"strict_disk_removal":true},"control_only":%t,"operation":%s}`, phase == "data_safe" || phase == "failed", operation)
}
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:8081" {
			return nil, fmt.Errorf("unexpected target %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return New(transport)
}
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestStrictAcceptanceRetryAndCompletion(t *testing.T) {
	var posts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			if r.Method != http.MethodGet {
				t.Error("state method")
			}
			phase := ""
			if posts.Load() > 0 {
				phase = "data_safe"
			}
			writeJSON(w, `{"shutdown":`+fixture(phase)+`}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/shutdown?mode=remove-disk" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong strict request", r.Method, r.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 2 || body["operation_id"] != "scale-in-42" || body["expected_generation"] != target.Generation {
			t.Error(body)
		}
		count := posts.Add(1)
		w.WriteHeader(http.StatusAccepted)
		phase := "draining"
		if count > 1 {
			phase = "data_safe"
		}
		writeJSON(w, fixture(phase))
	})
	for range 2 {
		accepted, err := c.RemoveDisk(t.Context(), target, "scale-in-42")
		if err != nil || !accepted.Accepted {
			t.Fatal(accepted, err)
		}
	}
	status, err := c.RemovalStatus(t.Context(), target, "scale-in-42")
	if err != nil || !status.DataSafe() {
		t.Fatal(status, err)
	}
	// Raw observation never becomes an unbound completion certificate.
	state, err := c.State(t.Context(), target)
	if err != nil || state.Shutdown.DataSafe() {
		t.Fatal(state, err)
	}
}

func TestRemovalStatusFailsClosed(t *testing.T) {
	cases := []struct {
		name, body          string
		wantError, wantSafe bool
	}{
		{"draining", fixture("draining"), false, false},
		{"completed", fixture("data_safe"), false, true},
		{"failed", fixture("failed"), false, false},
		{"deadline", strings.Replace(fixture("failed"), `"blocker":null`, `"blocker":"deadline exceeded"`, 1), false, false},
		{"lost operation", fixture(""), true, false},
		{"wrong runtime", strings.Replace(fixture("data_safe"), `"runtime_generation":"generation-a"`, `"runtime_generation":"generation-b"`, 1), true, false},
		{"wrong target", strings.Replace(fixture("data_safe"), `"expected_generation":"generation-a"`, `"expected_generation":"generation-b"`, 1), true, false},
		{"wrong operation", strings.ReplaceAll(fixture("data_safe"), "scale-in-42", "other"), true, false},
		{"wrong mode", strings.ReplaceAll(fixture("data_safe"), "remove-disk", "preserve"), true, false},
		{"not control only", strings.Replace(fixture("data_safe"), `"control_only":true`, `"control_only":false`, 1), true, false},
		{"missing control only", strings.Replace(fixture("data_safe"), `"control_only":true,`, "", 1), true, false},
		{"missing blocker", strings.Replace(fixture("data_safe"), `,"blocker":null`, "", 1), true, false},
		{"blocked safe", strings.Replace(fixture("data_safe"), `"blocker":null`, `"blocker":"still appending"`, 1), true, false},
		{"unknown phase", fixture("stopped"), true, false},
		{"unsupported", strings.ReplaceAll(fixture("data_safe"), `"strict_disk_removal":true`, `"strict_disk_removal":false`), true, false},
		{"unknown schema", strings.ReplaceAll(fixture("data_safe"), `"schema_version":1`, `"schema_version":2`), true, false},
		{"missing schema", strings.ReplaceAll(fixture("data_safe"), `"schema_version":1,`, ""), true, false},
		{"missing capability", strings.ReplaceAll(fixture("data_safe"), `"strict_disk_removal":true`, ""), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `{"shutdown":`+tc.body+`}`) })
			status, err := c.RemovalStatus(t.Context(), target, "scale-in-42")
			if (err != nil) != tc.wantError || status.DataSafe() != tc.wantSafe {
				t.Fatal(status, err)
			}
		})
	}
}

func TestStrictPreflightAndResponse(t *testing.T) {
	for _, scenario := range []string{"old API", "false capability", "unknown schema", "wrong generation", "conflict", "unsupported configuration", "old 200", "wrong response operation", "wrong response generation", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var posts atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					body := fixture("")
					switch scenario {
					case "old API":
						writeJSON(w, `{}`)
						return
					case "false capability":
						body = strings.ReplaceAll(body, `"strict_disk_removal":true`, `"strict_disk_removal":false`)
					case "unknown schema":
						body = strings.ReplaceAll(body, `"schema_version":1`, `"schema_version":2`)
					case "wrong generation":
						body = strings.ReplaceAll(body, "generation-a", "replacement")
					}
					writeJSON(w, `{"shutdown":`+body+`}`)
					return
				}
				posts.Add(1)
				body := fixture("draining")
				switch scenario {
				case "conflict":
					w.WriteHeader(409)
					return
				case "unsupported configuration":
					w.WriteHeader(501)
					return
				case "old 200":
					writeJSON(w, `{"ok":true}`)
					return
				case "wrong response operation":
					body = strings.ReplaceAll(body, "scale-in-42", "other")
				case "wrong response generation":
					body = strings.ReplaceAll(body, "generation-a", "replacement")
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(307)
					return
				}
				w.WriteHeader(202)
				writeJSON(w, body)
			})
			accepted, err := c.RemoveDisk(t.Context(), target, "scale-in-42")
			if err == nil || accepted.Accepted {
				t.Fatal(accepted, err)
			}
			if (scenario == "old API" || scenario == "false capability" || scenario == "unknown schema" || scenario == "wrong generation") && posts.Load() != 0 {
				t.Fatal("unsafe preflight posted")
			}
			if posts.Load() > 1 {
				t.Fatal("redirect followed")
			}
		})
	}
}

func TestOrdinaryOperations(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Error("unexpected method")
		}
		switch r.URL.RequestURI() {
		case "/shutdown", "/shutdown?handoff=preserve":
			writeJSON(w, `{"ok":true}`)
		case "/reload":
			writeJSON(w, `{"ok":true,"outcome":"adopted","generation":2,"version":"v2","prefix":"p/"}`)
		default:
			t.Error("unexpected URL", r.URL)
		}
	})
	ordinaryTarget := target
	ordinaryTarget.Generation = ""
	for _, mode := range []ShutdownMode{Ordinary, Preserve} {
		if result, err := c.Shutdown(t.Context(), ordinaryTarget, mode); err != nil || !result.Accepted {
			t.Fatal(result, err)
		}
	}
	if result, err := c.Reload(t.Context(), ordinaryTarget); err != nil || result.Generation == nil || *result.Generation != 2 {
		t.Fatal(result, err)
	}
	if _, err := c.Shutdown(t.Context(), target, Ordinary); !errors.Is(err, ErrUnsupported) {
		t.Fatal("ordinary shutdown cannot enforce generation", err)
	}
	if _, err := c.Reload(t.Context(), target); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := c.Shutdown(t.Context(), ordinaryTarget, ShutdownMode("remove-disk")); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func TestHTTPBoundaries(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized", "redirect", "duplicate", "nested duplicate", "trailing", "array", "null", "too deep", "canceled", "deadline", "timeout", "http failure"} {
		t.Run(scenario, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "malformed":
					writeJSON(w, `{`)
				case "oversized":
					writeJSON(w, strings.Repeat(" ", maxResponse+1))
				case "redirect":
					w.Header().Set("Location", "/state")
					w.WriteHeader(302)
				case "duplicate":
					writeJSON(w, `{"shutdown":null,"shutdown":null}`)
				case "nested duplicate":
					writeJSON(w, `{"unknown":{"a":1,"a":2}}`)
				case "trailing":
					writeJSON(w, `{} {}`)
				case "array":
					writeJSON(w, `[]`)
				case "null":
					writeJSON(w, `null`)
				case "too deep":
					writeJSON(w, `{"a":`+strings.Repeat("[", 70)+"0"+strings.Repeat("]", 70)+"}")
				case "deadline", "timeout":
					<-r.Context().Done()
				case "http failure":
					w.WriteHeader(503)
				default:
					writeJSON(w, `{}`)
				}
			})
			ctx := t.Context()
			if scenario == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if scenario == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			before := time.Now()
			_, err := c.State(ctx, Target{IP: target.IP})
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if time.Since(before) > callTimeout+time.Second {
				t.Fatal("call not bounded")
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if (scenario == "deadline" || scenario == "timeout") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestTargetAndOperationValidation(t *testing.T) {
	c := New(nil)
	for _, ip := range []string{"fleet-service", "127.0.0.1:8081", "http://127.0.0.1", ""} {
		if _, err := c.State(t.Context(), Target{IP: ip}); err == nil {
			t.Fatal(ip)
		}
	}
	for _, id := range []string{"", "with spaces", strings.Repeat("a", 129), "a/b"} {
		if _, err := c.RemoveDisk(t.Context(), target, id); err == nil {
			t.Fatal(id)
		}
	}
	incomplete := target
	incomplete.Generation = ""
	if _, err := c.RemoveDisk(t.Context(), incomplete, "op"); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}

// This fixture was emitted by the sibling runtime's actual State::snapshot,
// using its Control type. It is not a guessed or independently encoded schema.
func TestRuntimeGeneratedContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/shutdown-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for name, raw := range fixtures {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					writeJSON(w, string(raw))
					return
				}
				writeJSON(w, `{"shutdown":`+string(raw)+`}`)
			})
			snapshot, err := c.State(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Capabilities.SupportsRemoveDisk() != (name != "unsupported") {
				t.Fatal(snapshot)
			}
			status, err := c.RemovalStatus(t.Context(), target, "scale-in-42")
			if (err != nil) != (name == "idle" || name == "unsupported") || status.DataSafe() != (name == "data_safe") {
				t.Fatal(status, err)
			}
			if name != "idle" && name != "unsupported" {
				accepted, err := c.RemoveDisk(t.Context(), target, "scale-in-42")
				if err != nil || !accepted.Accepted {
					t.Fatal(accepted, err)
				}
			}
		})
	}
}

func TestStrictPOSTDeadlineIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var posted atomic.Bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, `{"shutdown":`+fixture("")+`}`)
			return
		}
		posted.Store(true)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	accepted, err := c.RemoveDisk(ctx, target, "scale-in-42")
	if !posted.Load() || !errors.Is(err, context.DeadlineExceeded) || accepted.Accepted {
		t.Fatal(accepted, err, posted.Load())
	}
	// A caller may retry the same operation. No completion is inferred from
	// timeout: the server may have accepted it before the connection was lost.
}

func TestControlOnlyStateWithoutActorLoad(t *testing.T) {
	// main.rs::handle_internal builds {} in control-only mode, then attaches
	// shutdown. The actor has stopped; terminal polling must not need its fields.
	for _, phase := range []string{"data_safe", "failed"} {
		t.Run(phase, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, `{"shutdown":`+fixture(phase)+`}`)
			})
			state, err := c.State(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := state.Capacity(time.Second); err == nil {
				t.Fatal("stopped actor produced a capacity sample")
			}
			status, err := c.RemovalStatus(t.Context(), target, "scale-in-42")
			if err != nil || status.Phase != phase || status.DataSafe() != (phase == "data_safe") {
				t.Fatal(status, err)
			}
		})
	}
}
