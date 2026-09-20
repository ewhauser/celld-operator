package v050

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// storeFixture is a primary bucket whose listing reports per-key ETags and
// sizes, the way ListObjectsV2 does, and whose reads report the ETag of the body
// they return. It counts every body read, so a test can prove exactly which
// records a pass went back to the bucket for.
type storeFixture struct {
	mu sync.Mutex
	// records maps a key to its body; etags is what the LISTING reports for it
	// and getETags what the READ reports. The two differ only where a test
	// injects an object rewritten between the list and the read.
	records  map[string][]byte
	etags    map[string]string
	getETags map[string]string
	sizes    map[string]int64 // overrides the listed size; otherwise the body length
	reads    []string
}

func newStore(t *testing.T, nodes ...string) *storeFixture {
	t.Helper()
	s := &storeFixture{records: map[string][]byte{}, etags: map[string]string{}, getETags: map[string]string{}, sizes: map[string]int64{}}
	for _, node := range nodes {
		s.put(t, node, 1)
	}
	return s
}

// put writes a node record and gives it the ETag of that version: a new version
// of the same node gets a new ETag, exactly as an S3 rewrite would.
func (s *storeFixture) put(t *testing.T, node string, version int) {
	t.Helper()
	body := mutate(t, fixture(t, "node-sealed"), func(m map[string]any) {
		m["node"] = node
		m["expires_ms"] = time.Now().Add(time.Hour).UnixMilli()
		m["log"].(map[string]any)["epoch"] = version
	})
	key := "nodes/" + node + ".json"
	etag := fmt.Sprintf("%q", fmt.Sprintf("%s-v%d", node, version))
	s.records[key] = body
	s.etags[key] = etag
	s.getETags[key] = etag
}
func (s *storeFixture) drop(node string) {
	key := "nodes/" + node + ".json"
	delete(s.records, key)
	delete(s.etags, key)
	delete(s.getETags, key)
	delete(s.sizes, key)
}
func (s *storeFixture) Get(ctx context.Context, key string) ([]byte, error) {
	data, _, err := s.GetETag(ctx, key)
	return data, err
}
func (s *storeFixture) GetETag(_ context.Context, key string) ([]byte, string, error) {
	s.mu.Lock()
	s.reads = append(s.reads, key)
	s.mu.Unlock()
	data, ok := s.records[key]
	if !ok {
		return nil, "", errors.New("missing record")
	}
	return data, s.getETags[key], nil
}

// List pages the key space the way the S3 reader does: at most 1000 keys, a
// continuation token naming the last key returned, and an explicitly complete
// final page carrying no token.
func (s *storeFixture) List(_ context.Context, prefix, continuation string) (Page, error) {
	page := Page{Complete: true}
	if prefix != "nodes/" {
		if continuation != "" {
			return Page{}, errors.New("unexpected continuation")
		}
		return page, nil
	}
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	start := 0
	if continuation != "" {
		index, found := slices.BinarySearch(keys, continuation)
		if !found {
			return Page{}, errors.New("unknown continuation token")
		}
		start = index + 1
	}
	if end := start + 1000; end < len(keys) {
		page = Page{Next: keys[end-1]}
		keys = keys[start:end]
	} else {
		keys = keys[start:]
	}
	for _, key := range keys {
		size := int64(len(s.records[key]))
		if override, ok := s.sizes[key]; ok {
			size = override
		}
		page.Keys = append(page.Keys, key)
		page.ETags = append(page.ETags, s.etags[key])
		page.Sizes = append(page.Sizes, size)
	}
	return page, nil
}

// taken returns the body reads since the last call, so each pass is measured on
// its own.
func (s *storeFixture) taken() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reads
	s.reads = nil
	return out
}

// plainStore is the pre-ETag Reader: it satisfies Reader and deliberately not
// ETagReader, so the adapter cannot confirm anything it serves.
type plainStore struct{ store *storeFixture }

func (p plainStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, _, err := p.store.GetETag(ctx, key)
	return data, err
}
func (p plainStore) List(ctx context.Context, prefix, continuation string) (Page, error) {
	return p.store.List(ctx, prefix, continuation)
}

func inventory(t *testing.T, a *Adapter, r Reader) (Inventory, error) {
	t.Helper()
	now := time.Now()
	return a.Inventory(t.Context(), r, func() time.Time { return now })
}

// The pinned runtime keeps folded records as sealed tombstones, so the listing
// names every invocation the fleet has ever had. An unchanged ETag must cost no
// GetObject at all.
func TestUnchangedETagsReadNoBodies(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a", "b", "c")
	first, err := inventory(t, a, store)
	if err != nil || len(first.Nodes) != 3 {
		t.Fatalf("first inventory: %+v %v", first, err)
	}
	if got := store.taken(); len(got) != 3 {
		t.Fatalf("first pass read %v", got)
	}
	second, err := inventory(t, a, store)
	if err != nil || len(second.Nodes) != 3 {
		t.Fatalf("second inventory: %+v %v", second, err)
	}
	if got := store.taken(); len(got) != 0 {
		t.Fatalf("unchanged records re-read: %v", got)
	}
	for i, node := range second.Nodes {
		if node.Name != first.Nodes[i].Name || node.Generation != first.Nodes[i].Generation || node.Epoch != first.Nodes[i].Epoch || node.LogState != first.Nodes[i].LogState || !slices.Equal(node.Ensemble, first.Nodes[i].Ensemble) {
			t.Fatalf("reused record differs: %+v %+v", node, first.Nodes[i])
		}
	}
}

// A rewritten record has a new ETag, and only that record is read again.
func TestChangedETagReadsExactlyThatRecord(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a", "b", "c")
	if _, err := inventory(t, a, store); err != nil {
		t.Fatal(err)
	}
	store.taken()
	store.put(t, "b", 2)
	out, err := inventory(t, a, store)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.taken(); len(got) != 1 || got[0] != "nodes/b.json" {
		t.Fatalf("expected one re-read of b, got %v", got)
	}
	for _, node := range out.Nodes {
		if node.Name == "b" && node.Epoch != 2 {
			t.Fatalf("stale record served after a rewrite: %+v", node)
		}
	}
}

// A key the listing no longer names is evicted, so a key that later reappears
// under its old ETag is still read: memory never resurrects a collected record.
func TestRemovedKeyIsEvictedAndRereadOnReturn(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a", "b")
	if _, err := inventory(t, a, store); err != nil {
		t.Fatal(err)
	}
	store.taken()
	store.drop("b")
	if out, err := inventory(t, a, store); err != nil || len(out.Nodes) != 1 {
		t.Fatalf("inventory after removal: %+v %v", out, err)
	}
	if got := store.taken(); len(got) != 0 {
		t.Fatalf("surviving record re-read: %v", got)
	}
	store.put(t, "b", 1)
	if out, err := inventory(t, a, store); err != nil || len(out.Nodes) != 2 {
		t.Fatalf("inventory after return: %+v %v", out, err)
	}
	if got := store.taken(); len(got) != 1 || got[0] != "nodes/b.json" {
		t.Fatalf("returning record not re-read: %v", got)
	}
}

// A body whose length contradicts the size listed against a MATCHING ETag is a
// real contradiction: the same version cannot have two lengths. It fails the
// pass and caches nothing, so the next reconcile starts from a fresh listing.
func TestReadContradictingTheListingFailsClosed(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a")
	store.sizes["nodes/a.json"] = int64(len(store.records["nodes/a.json"])) + 1
	out, err := inventory(t, a, store)
	if err == nil {
		t.Fatalf("contradiction accepted: %+v", out)
	}
	if !strings.Contains(err.Error(), "listing") && !strings.Contains(err.Error(), "listed") {
		t.Fatalf("unexpected failure: %v", err)
	}
	store.taken()
	if _, err := inventory(t, a, store); err == nil {
		t.Fatal("contradiction accepted on retry")
	}
	if got := store.taken(); len(got) != 1 {
		t.Fatalf("retry did not re-read: %v", got)
	}
}

// Every live writer rewrites its own record on each lease heartbeat, so on a
// link slow enough for the read to trail the listing the two ETags differ as a
// matter of course. That is an ordinary concurrent rewrite: the read returned
// one whole version of the object, so the pass uses it and must not fail. Only
// the cache key is stale, so nothing is remembered and the next pass reads it
// again. Treating this as a contradiction wedged every membership assessment
// under S3 latency, because a heartbeat almost always lands inside the window.
func TestHeartbeatRewriteBetweenListAndReadIsUsedNotRemembered(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a", "b")
	// The listing named version 1 of a; by the time its body arrives the writer
	// has published version 2. b is quiet and keeps the ETag it was listed with.
	store.records["nodes/a.json"] = mutate(t, fixture(t, "node-sealed"), func(m map[string]any) {
		m["node"] = "a"
		m["expires_ms"] = time.Now().Add(time.Hour).UnixMilli()
		m["log"].(map[string]any)["epoch"] = 2
	})
	store.getETags["nodes/a.json"] = `"a-v2"`
	out, err := inventory(t, a, store)
	if err != nil {
		t.Fatalf("heartbeat rewrite failed the pass: %v", err)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("records lost: %+v", out.Nodes)
	}
	index := slices.IndexFunc(out.Nodes, func(n Node) bool { return n.Name == "a" })
	if index < 0 || out.Nodes[index].Epoch != 2 {
		t.Fatalf("the body that was actually read was not used: %+v", out.Nodes)
	}
	if got := store.taken(); len(got) != 2 {
		t.Fatalf("first pass reads: %v", got)
	}
	// Nothing was remembered for a, so it is read again; b was confirmed and is
	// served from memory.
	if _, err := inventory(t, a, store); err != nil {
		t.Fatal(err)
	}
	if got := store.taken(); len(got) != 1 || got[0] != "nodes/a.json" {
		t.Fatalf("unconfirmed record remembered, or confirmed one dropped: %v", got)
	}
}

// A listing that reports no ETag proves nothing, and neither does a reader that
// cannot report the ETag of the body it served. Both re-read on every pass.
func TestRecordsWithoutAnETagAreAlwaysReread(t *testing.T) {
	for _, name := range []string{"listing", "reader"} {
		t.Run(name, func(t *testing.T) {
			a, _ := New(Image)
			store := newStore(t, "a", "b")
			var r Reader = store
			if name == "listing" {
				store.etags["nodes/a.json"] = ""
			} else {
				r = plainStore{store}
			}
			for pass := range 3 {
				if _, err := inventory(t, a, r); err != nil {
					t.Fatal(err)
				}
				expected := 2
				if name == "listing" && pass > 0 {
					expected = 1 // b still carries an ETag and is still reused
				}
				if got := store.taken(); len(got) != expected {
					t.Fatalf("pass %d read %v", pass, got)
				}
			}
		})
	}
}

// A body that does not parse is never remembered: the failure must recur from
// the primary bucket, and a repaired record must be read.
func TestParseFailureIsNotRemembered(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a")
	store.records["nodes/a.json"] = []byte(`{"node":"a"}`)
	for range 2 {
		if _, err := inventory(t, a, store); err == nil {
			t.Fatal("malformed record accepted")
		}
		if got := store.taken(); len(got) != 1 {
			t.Fatalf("parse failure served from memory: %v", got)
		}
	}
	store.put(t, "a", 2)
	out, err := inventory(t, a, store)
	if err != nil || len(out.Nodes) != 1 || out.Nodes[0].Epoch != 2 {
		t.Fatalf("repaired record not read: %+v %v", out, err)
	}
}

// The cache is bounded; past the bound the overflow is read on every pass.
func TestCacheIsBounded(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t)
	for i := range maxRecords + 8 {
		store.put(t, fmt.Sprintf("n%05d", i), 1)
	}
	if _, err := inventory(t, a, store); err != nil {
		t.Fatal(err)
	}
	store.taken()
	if _, err := inventory(t, a, store); err != nil {
		t.Fatal(err)
	}
	if got := len(store.taken()); got != 8 {
		t.Fatalf("expected the 8 uncached records to be re-read, got %d", got)
	}
	a.mu.Lock()
	held := len(a.records)
	a.mu.Unlock()
	if held != maxRecords {
		t.Fatalf("cache held %d records", held)
	}
}

// The manager reconciles up to four fleets at once and a caller may hand the
// same adapter to any of them, so the cache is exercised concurrently.
func TestConcurrentInventorySharesOneAdapter(t *testing.T) {
	a, _ := New(Image)
	store := newStore(t, "a", "b", "c")
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if out, err := inventory(t, a, store); err != nil || len(out.Nodes) != 3 {
				t.Errorf("concurrent inventory: %+v %v", out, err)
			}
		})
	}
	group.Wait()
}
