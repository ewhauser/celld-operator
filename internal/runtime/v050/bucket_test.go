package v050

import (
	"context"
	"errors"
	"strings"
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

func TestBucketRetiredLeaseIsMembershipEvidenceNotTermination(t *testing.T) {
	a, _ := New(Image)
	raw := fixture(t, "node-bucket")
	node, _ := a.ParseNode("nodes/a.json", raw)
	now := time.UnixMilli(int64(node.ExpiresMS)).Add(time.Second)
	for _, name := range []string{"expired", "live", "missing", "resolved missing", "resolved live", "resolved replacement", "resolved unreadable", "replacement", "log", "loss", "missing page"} {
		t.Run(name, func(t *testing.T) {
			r := &reader{data: raw, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
			members := []BucketMember{{Node: "a", Generation: node.Generation, Retired: true}}
			switch name {
			case "live", "resolved live":
				r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.Add(time.Minute).UnixMilli() })
			case "missing", "resolved missing":
				r.pages["nodes/"] = Page{Complete: true}
			case "replacement", "resolved replacement":
				members[0].Generation = "replaced"
			case "log":
				r.pages["log/"] = Page{Keys: []string{"log/old/peer.json"}, Complete: true}
			case "loss":
				r.pages["log/"] = Page{Keys: []string{"log/old/peer.loss.json"}, Complete: true}
			case "missing page":
				r.pages["log/"] = Page{Next: "missing"}
			}
			if strings.HasPrefix(name, "resolved") {
				members[0].Resolved = true
			}
			if name == "resolved unreadable" {
				r.fail = "get"
			}
			result, err := a.InspectBucketMembership(t.Context(), r, members, func() time.Time { return now })
			if name == "expired" || name == "resolved missing" {
				if err != nil || result.ObservedAt.IsZero() {
					t.Fatalf("%+v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("uncertainty became membership completion")
			}
		})
	}
}

func TestBucketExactSuccessorResolvesOnlyAdmittedHistory(t *testing.T) {
	a, _ := New(Image)
	raw := fixture(t, "node-bucket")
	node, _ := a.ParseNode("nodes/a.json", raw)
	now := time.UnixMilli(node.SampledMS)
	for _, name := range []string{"successor", "old revival", "unknown replacement", "missing successor", "expired successor", "partial listing", "chain", "cycle", "unknown historical writer"} {
		t.Run(name, func(t *testing.T) {
			r := &reader{data: raw, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
			members := []BucketMember{{Node: "a", Generation: "old", SupersededBy: node.Generation, Retired: true}, {Node: "a", Generation: node.Generation}}
			switch name {
			case "old revival":
				r.data = mutate(t, raw, func(m map[string]any) { m["ownership_index_generation"] = "old" })
			case "unknown replacement":
				r.data = mutate(t, raw, func(m map[string]any) { m["ownership_index_generation"] = "unknown" })
			case "missing successor":
				r.pages["nodes/"] = Page{Complete: true}
			case "expired successor":
				r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.UnixMilli() })
			case "partial listing":
				r.pages["nodes/"] = Page{Next: "missing"}
			case "chain":
				members = append(members, BucketMember{Node: "a", Generation: "earlier", SupersededBy: "old", Retired: true})
			case "cycle":
				members[0].SupersededBy = "old"
			case "unknown historical writer":
				members = append(members, BucketMember{Node: "a", Generation: "unadmitted", Retired: true})
			}
			_, err := a.InspectBucketMembership(t.Context(), r, members, func() time.Time { return now })
			if (name == "successor" || name == "chain") != (err == nil) {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}

// The retirement probe is a narrowing of the full assessment, so it must accept
// exactly what InspectBucketMembership accepts about a single retired record --
// and nothing else. It reports positives only: a record that is absent,
// unreadable, superseded, replaced, still live, or carrying peer-recovery state
// yields no observation, so the probe can neither manufacture authority nor
// revoke it.
func TestObserveBucketRetirementReportsOnlyPositiveExpiry(t *testing.T) {
	a, _ := New(Image)
	raw := fixture(t, "node-bucket")
	n, err := a.ParseNode("nodes/a.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(n.SampledMS)
	for _, name := range []string{"expired", "elapsed exactly", "live lease", "missing record", "unreadable", "replaced generation", "peer session", "unknown writer"} {
		t.Run(name, func(t *testing.T) {
			r := &reader{data: raw, pages: map[string]Page{"nodes/": {Keys: []string{"nodes/a.json"}, Complete: true}, "log/": {Complete: true}}}
			members := []BucketMember{{Node: n.Name, Generation: n.Generation, Retired: true}}
			// The lease elapsed a second ago: this is the window the probe exists
			// to catch, and every other case must come back empty.
			r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.Add(-time.Second).UnixMilli() })
			switch name {
			case "elapsed exactly":
				r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.UnixMilli() })
			case "live lease":
				r.data = mutate(t, raw, func(m map[string]any) { m["expires_ms"] = now.Add(time.Minute).UnixMilli() })
			case "missing record":
				r.pages["nodes/"] = Page{Complete: true}
			case "unreadable":
				r.fail = "get"
			case "replaced generation":
				members[0].Generation = "different"
			case "peer session":
				r.data = fixture(t, "node-open")
			case "unknown writer":
				members[0].Node = "b"
			}
			observed, err := a.ObserveBucketRetirement(t.Context(), r, members, func() time.Time { return now })
			if name == "unreadable" {
				if err == nil {
					t.Fatal("an unreadable record was not reported as a transport failure")
				}
				return
			}
			if err != nil {
				t.Fatalf("probe failed: %v", err)
			}
			want := 0
			if name == "expired" || name == "elapsed exactly" {
				want = 1
			}
			if len(observed) != want {
				t.Fatalf("observed %d expired records, want %d: %+v", len(observed), want, observed)
			}
			if want == 1 && (observed[0].Node != n.Name || observed[0].Generation != n.Generation) {
				t.Fatalf("observation names the wrong writer: %+v", observed[0])
			}
		})
	}
}

// A superseded predecessor is resolved through succession, never through its own
// record, which the successor has overwritten. Offering one is a programming
// error rather than a silent no-op.
func TestObserveBucketRetirementRefusesAmbiguousProbes(t *testing.T) {
	a, _ := New(Image)
	r := &reader{data: fixture(t, "node-bucket"), pages: map[string]Page{"nodes/": {Complete: true}, "log/": {Complete: true}}}
	now := time.Unix(10000, 0)
	for _, members := range [][]BucketMember{
		{{Node: "a", Generation: "one", SupersededBy: "two", Retired: true}},
		{{Node: "a", Generation: "one", Retired: true}, {Node: "a", Generation: "two", Retired: true}},
		{{Node: "", Generation: "one", Retired: true}},
	} {
		if _, err := a.ObserveBucketRetirement(t.Context(), r, members, func() time.Time { return now }); err == nil {
			t.Fatalf("ambiguous probe accepted: %+v", members)
		}
	}
	if observed, err := a.ObserveBucketRetirement(t.Context(), r, nil, func() time.Time { return now }); err != nil || observed != nil {
		t.Fatalf("empty probe read anything: %+v %v", observed, err)
	}
}
