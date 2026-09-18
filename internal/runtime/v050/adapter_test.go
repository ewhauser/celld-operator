package v050

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func mutate(t *testing.T, b []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestPin(t *testing.T) {
	for _, image := range []string{Image, ARM64, AMD64} {
		if _, err := New(image); err != nil {
			t.Fatal(err)
		}
	}
	for _, image := range []string{"", "v0.5.0", "ghcr.io/denoland/celld:latest"} {
		if _, err := New(image); err == nil {
			t.Fatal("accepted tag")
		}
	}
}
func TestNode(t *testing.T) {
	a, _ := New(Image)
	b := fixture(t, "node-sealed")
	n, err := a.ParseNode("nodes/a.json", b)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "a" || n.LogState != "sealed" || n.Generation == "" {
		t.Fatalf("bad node: %+v", n)
	}
	cases := map[string]func(map[string]any){
		"missing node":     func(m map[string]any) { delete(m, "node") },
		"null identity":    func(m map[string]any) { m["node"] = nil },
		"wrong identity":   func(m map[string]any) { m["node"] = "b" },
		"traversal":        func(m map[string]any) { m["node"] = "../a" },
		"missing expiry":   func(m map[string]any) { delete(m, "expires_ms") },
		"negative expiry":  func(m map[string]any) { m["expires_ms"] = -1 },
		"protocol":         func(m map[string]any) { m["peer_protocol"] = 6 },
		"missing protocol": func(m map[string]any) { delete(m, "peer_protocol") },
		"no generation":    func(m map[string]any) { delete(m, "ownership_index_generation"); delete(m, "probe_public_key") },
		"null generation":  func(m map[string]any) { m["ownership_index_generation"] = nil },
		"unknown state":    func(m map[string]any) { m["log"].(map[string]any)["state"] = "complete" },
		"missing active":   func(m map[string]any) { delete(m["log"].(map[string]any), "active") },
		"zero epoch":       func(m map[string]any) { m["log"].(map[string]any)["epoch"] = 0 },
		"missing epoch":    func(m map[string]any) { delete(m["log"].(map[string]any), "epoch") },
		"null ensemble":    func(m map[string]any) { m["log"].(map[string]any)["ensemble"] = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.ParseNode("nodes/a.json", mutate(t, b, change)); err == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
	for _, bad := range [][]byte{[]byte(`{"node":"a","node":"a"}`), append(append([]byte{}, b...), []byte(` {}`)...), []byte(`null`), []byte(`{`), []byte(strings.Repeat(" ", maxJSON+1))} {
		if _, err := a.ParseNode("nodes/a.json", bad); err == nil {
			t.Fatal("accepted malformed JSON")
		}
	}
	fallback := mutate(t, b, func(m map[string]any) { m["ownership_index_generation"] = "" })
	if _, err := a.ParseNode("nodes/a.json", fallback); err != nil {
		t.Fatal(err)
	}
}
func TestState(t *testing.T) {
	a, _ := New(Image)
	b := fixture(t, "state")
	var raw struct {
		Load struct {
			Sampled int64 `json:"sampled_ms"`
		} `json:"node_load"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(raw.Load.Sampled).Add(time.Second)
	s, err := a.ParseState(200, b, now, now, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.SampledAt.UnixMilli() != raw.Load.Sampled {
		t.Fatal("sample changed")
	}
	for _, field := range []string{"capacity_waiting", "activation_waiting", "restoring", "occupied", "owned_cells", "node_load"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []any{nil, "unknown", -1} {
				bad := mutate(t, b, func(m map[string]any) { m[field] = value })
				if _, err := a.ParseState(200, bad, now, now, 5*time.Second); err == nil {
					t.Fatal("accepted invalid observation")
				}
			}
		})
	}
	for _, field := range []string{"draining", "rebalance_paused", "pressured", "memory_headroom", "sampled_ms", "resident_cells", "rss_bytes", "in_use_bytes"} {
		bad := mutate(t, b, func(m map[string]any) { delete(m["node_load"].(map[string]any), field) })
		if _, err := a.ParseState(200, bad, now, now, 5*time.Second); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	for _, tc := range []struct {
		code         int
		received, at time.Time
		age          time.Duration
	}{
		{503, now, now, time.Second}, {200, now.Add(-time.Minute), now, time.Second},
		{200, now, now.Add(time.Minute), time.Second}, {200, now, now, 0},
		{200, now, now.Add(-time.Minute), time.Second},
	} {
		if _, err := a.ParseState(tc.code, b, tc.received, tc.at, tc.age); err == nil {
			t.Fatal("accepted stale/unavailable state")
		}
	}
}

type reader struct {
	data  []byte
	pages map[string]Page
	fail  string
	calls []string
}

func (r *reader) Get(_ context.Context, key string) ([]byte, error) {
	r.calls = append(r.calls, "get "+key)
	if r.fail == "get" {
		return nil, errors.New("AccessDenied")
	}
	return r.data, nil
}
func (r *reader) List(_ context.Context, prefix, token string) (Page, error) {
	r.calls = append(r.calls, "list "+prefix+token)
	if r.fail == prefix {
		return Page{}, errors.New("listing failed")
	}
	return r.pages[prefix+token], nil
}
func TestRecovery(t *testing.T) {
	a, _ := New(Image)
	b := fixture(t, "node-sealed")
	n, err := a.ParseNode("nodes/a.json", b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	for _, name := range []string{"complete", "open", "recovering", "absent log", "null log", "generation replaced", "prior unresolved", "not stopped", "missing inventory", "lost history", "missing operation", "stale", "future", "budget", "get denied", "list denied", "loss old generation", "bundle loss", "second page loss", "loop", "duplicate key", "foreign prefix", "epoch rewind", "assessment expires", "canceled", "truncated", "ambiguous final"} {
		t.Run(name, func(t *testing.T) {
			r := &reader{data: b, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
			req := Request{OperationID: "op-1", Sessions: []Session{{Node: "a", Generation: n.Generation, Epoch: n.Epoch, Stopped: true}}, InventoryComplete: true, CapturedAt: now, MaxAge: time.Second, PageBudget: 10}
			clock := func() time.Time { return now }
			ctx := t.Context()
			switch name {
			case "open", "recovering":
				r.data = mutate(t, b, func(m map[string]any) { m["log"].(map[string]any)["state"] = name })
			case "absent log":
				r.data = mutate(t, b, func(m map[string]any) { delete(m, "log") })
			case "null log":
				r.data = mutate(t, b, func(m map[string]any) { m["log"] = nil })
			case "generation replaced":
				req.Sessions[0].Generation = "different"
			case "prior unresolved":
				req.Sessions = append(req.Sessions, Session{Node: "a", Generation: "previous", Stopped: true})
			case "not stopped":
				req.Sessions[0].Stopped = false
			case "missing inventory":
				r.pages["nodes/"] = Page{}
			case "lost history":
				req.InventoryComplete = false
			case "missing operation":
				req.OperationID = ""
			case "stale":
				req.CapturedAt = now.Add(-time.Minute)
			case "future":
				req.CapturedAt = now.Add(time.Minute)
			case "budget":
				req.PageBudget = 1
			case "get denied":
				r.fail = "get"
			case "list denied":
				r.fail = "log/"
			case "loss old generation":
				r.pages["log/"] = Page{Complete: true, Keys: []string{"log/old/old.e1.loss.json"}}
			case "bundle loss":
				r.pages["log/"] = Page{Complete: true, Keys: []string{"log/a/old.bundle-1.loss.json"}}
			case "second page loss":
				r.pages["log/"] = Page{Next: "next"}
				r.pages["log/next"] = Page{Complete: true, Keys: []string{"log/old/old.e1.loss.json"}}
			case "loop":
				r.pages["log/"] = Page{Next: "next"}
				r.pages["log/next"] = Page{Next: "next"}
			case "duplicate key":
				r.pages["nodes/"] = Page{Complete: true, Keys: []string{"nodes/a.json", "nodes/a.json"}}
			case "foreign prefix":
				r.pages["log/"] = Page{Complete: true, Keys: []string{"other/record"}}
			case "epoch rewind":
				req.Sessions[0].Epoch++
			case "assessment expires":
				calls := 0
				clock = func() time.Time { calls++; return now.Add(time.Duration(calls-1) * time.Minute) }
			case "truncated":
				r.pages["log/"] = Page{}
			case "ambiguous final":
				r.pages["log/"] = Page{Complete: true, Next: "next"}
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			result, err := a.Assess(ctx, r, req, clock)
			switch {
			case name == "complete":
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Completed) != 1 || result.OperationID != "op-1" {
					t.Fatalf("bad evidence: %+v", result)
				}
				if !reflect.DeepEqual(r.calls, []string{"list nodes/", "get nodes/a.json", "list log/"}) {
					t.Fatalf("wrong evidence order: %v", r.calls)
				}
			case err == nil:
				t.Fatalf("accepted %s: %+v", name, result)
			case !reflect.DeepEqual(result, Evidence{}):
				t.Fatal("partial success on failure")
			}
		})
	}
}

func TestObservedNonCompletion(t *testing.T) {
	a, _ := New(Image)
	for _, tc := range []struct{ file, key, state string }{
		{"node-open", "nodes/a.json", "open"},
		{"node-deadline-open", "nodes/b.json", "open"},
		{"node-bucket", "nodes/a.json", ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			n, err := a.ParseNode(tc.key, fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if n.LogState != tc.state {
				t.Fatalf("got state %q", n.LogState)
			}
		})
	}
}

func TestCompletePagination(t *testing.T) {
	r := &reader{pages: map[string]Page{
		"log/":      {Keys: []string{"log/a/bundle"}, Next: "page2"},
		"log/page2": {Keys: []string{"log/b/bundle"}, Complete: true},
	}}
	budget := 2
	keys, err := list(t.Context(), r, "log/", &budget)
	if err != nil || len(keys) != 2 || budget != 0 {
		t.Fatalf("pagination: %v %v budget=%d", keys, err, budget)
	}
}

func TestCancellationDuringFinalPage(t *testing.T) {
	a, _ := New(Image)
	b := fixture(t, "node-sealed")
	n, err := a.ParseNode("nodes/a.json", b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &cancelFinalReader{cancel: cancel}
	r.data = b
	r.pages = map[string]Page{
		"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true},
		"log/":   {Complete: true},
	}
	req := Request{OperationID: "op", Sessions: []Session{{Node: n.Name, Generation: n.Generation, Epoch: n.Epoch, Stopped: true}}, InventoryComplete: true, CapturedAt: now, MaxAge: time.Minute, PageBudget: 10}
	if _, err := a.Assess(ctx, r, req, func() time.Time { return now }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

type cancelFinalReader struct {
	reader
	cancel context.CancelFunc
}

func (r *cancelFinalReader) List(ctx context.Context, prefix, token string) (Page, error) {
	page, err := r.reader.List(ctx, prefix, token)
	if prefix == "log/" {
		r.cancel()
	}
	return page, err
}

func TestInspectLivePreflightStillChecksHistory(t *testing.T) {
	a, _ := New(Image)
	b := fixture(t, "node-open")
	n, err := a.ParseNode("nodes/a.json", b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(int64(n.ExpiresMS) - 1)
	req := Request{OperationID: "preflight", Sessions: []Session{{Node: n.Name, Generation: n.Generation, Epoch: n.Epoch}}, InventoryComplete: true, CapturedAt: now, MaxAge: time.Second, PageBudget: 10}
	r := &reader{data: b, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
	if _, err := a.Inspect(t.Context(), r, req, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Assess(t.Context(), r, req, func() time.Time { return now }); err == nil {
		t.Fatal("live preflight counted as stopped completion")
	}
	r.pages["log/"] = Page{Keys: []string{"log/old/previous.e1.loss.json"}, Complete: true}
	if _, err := a.Inspect(t.Context(), r, req, func() time.Time { return now }); err == nil {
		t.Fatal("preflight ignored historical loss")
	} else if _, ok := errors.AsType[*LossError](err); !ok {
		t.Fatalf("loss not classified: %v", err)
	}
}
