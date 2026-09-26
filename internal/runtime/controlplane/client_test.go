package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var target = Target{IP: "127.0.0.1"}

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

func TestHTTPBoundaries(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized", "redirect", "duplicate", "nested duplicate", "trailing", "array", "null", "too deep", "canceled", "deadline", "timeout", "http failure", "connection closed", "truncated body", "truncated rejection"} {
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
				case "connection closed":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				case "truncated body", "truncated rejection":
					w.Header().Set("Content-Length", "1000")
					if scenario == "truncated rejection" {
						w.WriteHeader(503)
					}
					writeJSON(w, `{}`)
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
			_, err := c.State(ctx, target)
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

func TestStateRequiresExactNodeIP(t *testing.T) {
	c := New(nil)
	for _, ip := range []string{"fleet-service", "127.0.0.1:8081", "http://127.0.0.1", ""} {
		if _, err := c.State(t.Context(), Target{IP: ip}); err == nil {
			t.Fatal(ip)
		}
	}
}

// Capacity is read only from a runtime that reports schema 1 of the shutdown
// object and its runtime generation, and only with the actor's load fields.
func TestCapacityRequiresRuntimeIdentity(t *testing.T) {
	captured, err := os.ReadFile("testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	current := mutate(t, captured, func(m map[string]any) {
		m["node_load"].(map[string]any)["sampled_ms"] = time.Now().UnixMilli()
	})
	sample := func(t *testing.T, body string) (State, error) {
		t.Helper()
		c := testClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
		state, err := c.State(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		_, err = state.Capacity(10 * time.Second)
		return state, err
	}
	if state, err := sample(t, string(current)); err != nil || state.Generation != "generation-a" {
		t.Fatal("captured runtime state rejected", state, err)
	}
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"no shutdown":    {`{}`, ErrUnsupported},
		"null shutdown":  {`{"shutdown":null}`, ErrUnsupported},
		"unknown schema": {`{"shutdown":{"schema_version":2,"runtime_generation":{"future":true}}}`, ErrUnsupported},
		"no generation":  {`{"shutdown":{"schema_version":1}}`, ErrIdentity},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sample(t, tc.body); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
	// A stopped actor reports only the shutdown object: an identity, no load.
	if _, err := sample(t, `{"shutdown":{"schema_version":1,"runtime_generation":"generation-a"}}`); err == nil {
		t.Fatal("stopped actor produced a capacity sample")
	}
}
