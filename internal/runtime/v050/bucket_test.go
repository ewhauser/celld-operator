package v050

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBucketPreflightIsNotPeerRecovery(t *testing.T) {
	a, _ := New(Image)
	raw := fixture(t, "node-bucket")
	n, err := a.ParseNode("nodes/a.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(n.SampledMS)
	for _, name := range []string{"complete", "peer session", "peer history", "later loss", "missing node", "missing body", "replacement", "historical generation", "stopped", "expired lease", "stale sample", "unknown inventory", "partial listing", "duplicate listing", "canceled", "deadline", "missing capture"} {
		t.Run(name, func(t *testing.T) {
			r := &reader{data: raw, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
			req := Request{OperationID: "bucket-op", Sessions: []Session{{Node: n.Name, Generation: n.Generation}}, InventoryComplete: true, CapturedAt: now, MaxAge: 5 * time.Second, PageBudget: 10}
			at := now
			clock := func() time.Time { return at }
			ctx := t.Context()
			switch name {
			case "peer session":
				r.data = fixture(t, "node-open")
			case "peer history":
				r.pages["log/"] = Page{Keys: []string{"log/old/generation.e1.json"}, Complete: true}
			case "later loss":
				r.pages["log/"] = Page{Keys: []string{"log/old/generation.e1.json"}, Next: "next"}
				r.pages["log/next"] = Page{Keys: []string{"log/old/generation.e1.loss.json"}, Complete: true}
			case "missing node":
				r.pages["nodes/"] = Page{Complete: true}
			case "missing body":
				r.fail = "get"
			case "replacement":
				req.Sessions[0].Generation = "different"
			case "historical generation":
				req.Sessions = append(req.Sessions, Session{Node: n.Name, Generation: "previous"})
			case "stopped":
				req.Sessions[0].Stopped = true
			case "expired lease":
				r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.UnixMilli() })
			case "stale sample":
				r.data = mutate(t, raw, func(m map[string]any) { m["load"].(map[string]any)["sampled_ms"] = now.Add(-time.Minute).UnixMilli() })
			case "unknown inventory":
				req.InventoryComplete = false
			case "partial listing":
				r.pages["log/"] = Page{Next: "missing"}
			case "duplicate listing":
				r.pages["nodes/"] = Page{Keys: []string{"nodes/a.json", "nodes/a.json"}, Complete: true}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				clock = func() time.Time { at = at.Add(2 * time.Second); return at }
			case "missing capture":
				req.CapturedAt = time.Time{}
			}
			observation, err := a.InspectBucket(ctx, r, req, clock)
			if name == "complete" {
				if err != nil || !observation.ObservedAt.Equal(now) {
					t.Fatalf("%+v %v", observation, err)
				}
				// The same absent log cannot complete a stopped peer recovery request.
				req.Sessions[0].Stopped = true
				if _, err := a.Assess(ctx, r, req, clock); err == nil {
					t.Fatal("Bucket observation became peer recovery proof")
				}
			} else if err == nil || !observation.ObservedAt.IsZero() {
				t.Fatalf("accepted %s: %+v %v", name, observation, err)
			}
			if name == "later loss" {
				if _, ok := errors.AsType[*LossError](err); !ok {
					t.Fatalf("loss masked: %v", err)
				}
			}
		})
	}
}
