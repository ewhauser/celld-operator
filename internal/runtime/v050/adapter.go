// Package v050 interprets the private celld v0.5.0 evidence schema.
// Candidate completion is not authorization to remove a process or a disk.
package v050

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	Commit  = "12d5b6333fe52717325addcfe1e99e9fd4f77bcd"
	Image   = "ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8"
	ARM64   = "ghcr.io/denoland/celld@sha256:0c915aed95925945d145f242811b95cd4a6659ddd577dfbb6104c00c447b1e43"
	AMD64   = "ghcr.io/denoland/celld@sha256:3cb29128109213d30570c6503ccfd94cee1cd268be0e795d8cc04f448f6d91a8"
	maxJSON = 1 << 20
)

// Adapter must be selected using a verified image identity, never a mutable tag.
// It also carries the ETag-gated record cache (records.go), so a caller that
// keeps one Adapter per fleet across reconciles re-reads only the node records
// whose listed ETag changed. The zero Adapter is a valid, empty cache.
type Adapter struct {
	// mu guards records: the manager reconciles up to four fleets concurrently,
	// and a caller may hand the same Adapter to any of them.
	mu      sync.Mutex
	records map[string]cachedRecord
}

func New(image string) (*Adapter, error) {
	if image != Image && image != ARM64 && image != AMD64 {
		return nil, errors.New("unsupported runtime image")
	}
	return &Adapter{}, nil
}

// object rejects duplicate keys, including nested keys: encoding/json otherwise
// silently accepts the last occurrence. Extra fields remain opaque in this pin.
func object(data []byte) (map[string]json.RawMessage, error) {
	if len(data) > maxJSON {
		return nil, errors.New("metadata exceeds budget")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := unique(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("expected object")
	}
	return m, nil
}
func unique(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return errors.New("invalid or duplicate JSON key")
			}
			seen[key] = true
			if err := unique(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := unique(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}
func required(m map[string]json.RawMessage, key string, target any) error {
	raw, ok := m[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("missing %s", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid %s: %w", key, err)
	}
	return nil
}
func fresh(sample, now time.Time, age time.Duration) bool {
	return age > 0 && !sample.IsZero() && !sample.After(now) && now.Sub(sample) <= age
}

type State struct {
	CapacityWaiting, ActivationWaiting, Restoring, Occupied, OwnedCells uint64
	ResidentCells, RSSBytes, InUseBytes                                 uint64
	Draining, RebalancePaused, Pressured, MemoryHeadroom                bool
	SampledAt                                                           time.Time
}

// ParseState validates required capacity fields; optional cgroup measurements
// are intentionally not converted to zero. HTTP status is part of the evidence.
func (*Adapter) ParseState(status int, data []byte, received, now time.Time, age time.Duration) (State, error) {
	var s State
	if status != 200 || !fresh(received, now, age) {
		return s, errors.New("unavailable or stale HTTP state")
	}
	m, err := object(data)
	if err != nil {
		return s, err
	}
	for k, p := range map[string]*uint64{"capacity_waiting": &s.CapacityWaiting, "activation_waiting": &s.ActivationWaiting, "restoring": &s.Restoring, "occupied": &s.Occupied, "owned_cells": &s.OwnedCells} {
		if err := required(m, k, p); err != nil {
			return s, err
		}
	}
	load, err := object(m["node_load"])
	if err != nil {
		return s, err
	}
	var sampled int64
	for k, p := range map[string]any{"draining": &s.Draining, "rebalance_paused": &s.RebalancePaused, "sampled_ms": &sampled, "resident_cells": &s.ResidentCells, "rss_bytes": &s.RSSBytes, "in_use_bytes": &s.InUseBytes, "pressured": &s.Pressured, "memory_headroom": &s.MemoryHeadroom} {
		if err := required(load, k, p); err != nil {
			return s, err
		}
	}
	s.SampledAt = time.UnixMilli(sampled)
	if !fresh(s.SampledAt, now, age) {
		return s, errors.New("stale or future runtime sample")
	}
	return s, nil
}

type Node struct {
	Name, Generation, LogState string
	Address                    string
	SampledMS                  int64
	ExpiresMS, Epoch, Tiered   uint64
	Ensemble                   []string
}

var identity = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func validID(s string) bool { return identity.MatchString(s) && s != "." && s != ".." }

func (*Adapter) ParseNode(key string, data []byte) (Node, error) {
	var n Node
	m, err := object(data)
	if err != nil {
		return n, err
	}
	if err := required(m, "node", &n.Name); err != nil {
		return n, err
	}
	if !validID(n.Name) || key != "nodes/"+n.Name+".json" {
		return n, errors.New("node/key mismatch")
	}
	if err := required(m, "expires_ms", &n.ExpiresMS); err != nil {
		return n, err
	}
	if raw, ok := m["addr"]; ok {
		if err := json.Unmarshal(raw, &n.Address); err != nil {
			return n, err
		}
	}
	if raw, ok := m["load"]; ok {
		var load struct {
			SampledMS int64 `json:"sampled_ms"`
		}
		if err := json.Unmarshal(raw, &load); err != nil {
			return n, err
		}
		n.SampledMS = load.SampledMS
	}
	var protocol uint16
	if err := required(m, "peer_protocol", &protocol); err != nil {
		return n, err
	}
	if protocol != 5 {
		return n, errors.New("unsupported peer protocol")
	}
	if raw, ok := m["ownership_index_generation"]; ok {
		if err := json.Unmarshal(raw, &n.Generation); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return n, errors.New("invalid generation")
		}
	}
	if n.Generation == "" {
		if err := required(m, "probe_public_key", &n.Generation); err != nil {
			return n, err
		}
	}
	if !validID(n.Generation) {
		return n, errors.New("invalid generation identity")
	}
	raw, ok := m["log"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return n, nil
	} // parsed, but NOT completion
	log, err := object(raw)
	if err != nil {
		return n, err
	}
	var active bool
	for k, p := range map[string]any{"active": &active, "state": &n.LogState, "epoch": &n.Epoch, "ensemble": &n.Ensemble, "tiered": &n.Tiered} {
		if err := required(log, k, p); err != nil {
			return n, err
		}
	}
	if n.Epoch == 0 {
		return n, errors.New("invalid zero log epoch")
	}
	switch n.LogState {
	case "open", "recovering", "sealed":
	default:
		return n, errors.New("unsupported log state")
	}
	for _, peer := range n.Ensemble {
		if !validID(peer) {
			return n, errors.New("invalid ensemble")
		}
	}
	return n, nil
}

// Reader is deliberately read-only and relative to one configured primary fleet
// prefix. Implementations must bypass caches; partial/error pages are errors.
// Complete must be explicit on the final page. No log/ bodies are requested.
type Reader interface {
	Get(context.Context, string) ([]byte, error)
	List(ctx context.Context, prefix, continuation string) (Page, error)
}

// ETagReader is an optional Reader extension: GetETag returns a body together
// with the ETag the store reported for that exact body. A Reader that does not
// implement it can never satisfy the reuse rules in readNodes, so every record
// it serves is read on every pass, exactly as before this extension existed.
type ETagReader interface {
	Reader
	GetETag(ctx context.Context, key string) ([]byte, string, error)
}
type Page struct {
	Keys []string
	// ETags and Sizes are optional listing metadata, each either empty or exactly
	// parallel to Keys: ETags[i] and Sizes[i] describe Keys[i]. An empty ETag, or
	// a listing that reports none at all, means "content unknown" and forces a
	// body read; a zero Size means "size not reported". A Reader that reports
	// neither keeps the pre-ETag behavior of re-reading every record.
	ETags    []string
	Sizes    []int64
	Next     string
	Complete bool // explicitly confirmed final page; zero-value pages fail closed
}
type Session struct {
	Node, Generation string
	Epoch            uint64
	// Stopped must mean confirmed process termination/fencing, not Pod deletion.
	Stopped bool
}
type Request struct {
	OperationID       string
	Sessions          []Session // complete inventory, including all unresolved prior sessions
	InventoryComplete bool      // caller must reconstruct durable history first
	CapturedAt        time.Time
	MaxAge            time.Duration
	PageBudget        int
}

// incomplete fails closed on a request that cannot support any assessment: no
// operation, unreconstructed history, no sessions, no listing budget, or a
// capture that is already stale. now is taken as a func so the clock is read
// only when the cheaper field checks have all passed, as before.
func (req Request) incomplete(now func() time.Time) bool {
	return req.OperationID == "" || !req.InventoryComplete || len(req.Sessions) == 0 || req.PageBudget <= 0 || !fresh(req.CapturedAt, now(), req.MaxAge)
}

type Evidence struct {
	OperationID string
	Completed   []Session
	ObservedAt  time.Time
}

// Assess returns candidate evidence only. The caller must persist its session
// ledger, serialize disruptions, revalidate membership/metrics, retain disks,
// and qualify the failure model before using it. Generation replacement and
// absent logs deliberately remain blocked even when source suggests recovery.
func (a *Adapter) Assess(ctx context.Context, r Reader, req Request, now func() time.Time) (Evidence, error) {
	return a.assess(ctx, r, req, now, true)
}

// Inspect checks pre-removal inventory and all historical recovery/loss evidence.
func (a *Adapter) Inspect(ctx context.Context, r Reader, req Request, now func() time.Time) (Evidence, error) {
	return a.assess(ctx, r, req, now, false)
}

func (a *Adapter) assess(ctx context.Context, r Reader, req Request, now func() time.Time, requireStopped bool) (Evidence, error) {
	var result Evidence
	if req.incomplete(now) {
		return Evidence{}, errors.New("incomplete lifecycle evidence")
	}
	ctx, cancel := context.WithTimeout(ctx, req.MaxAge)
	defer cancel()
	expected := map[string]Session{}
	for _, s := range req.Sessions {
		if !validID(s.Node) || !validID(s.Generation) {
			return Evidence{}, errors.New("invalid session")
		}
		if _, exists := expected[s.Node]; exists {
			return Evidence{}, errors.New("multiple generations unresolved")
		}
		expected[s.Node] = s
	}
	budget := req.PageBudget
	nodes, err := a.readNodes(ctx, r, &budget, len(expected), errors.New("inventory changed or missing nodes"))
	if err != nil {
		return Evidence{}, err
	}
	for _, n := range nodes {
		s, ok := expected[n.Name]
		if !ok || s.Generation != n.Generation {
			return Evidence{}, errors.New("unknown node or generation replacement")
		}
		// Deliberately ahead of the Stopped split: a rewound or missing epoch
		// disqualifies a live session too, so this must not move inside the
		// stopped branch below. That branch therefore needs no epoch check of
		// its own; n.Epoch >= s.Epoch already holds for every node past here.
		if n.Epoch < s.Epoch {
			return Evidence{}, errors.New("recovery epoch rewound or log missing")
		}
		// One clock read decides both halves of the lease test: two reads let the
		// sign check and the expiry comparison disagree about "now".
		if !s.Stopped {
			at := now().UnixMilli()
			if at < 0 || n.ExpiresMS <= uint64(at) {
				return Evidence{}, errors.New("unresolved unavailable session")
			}
		}
		if s.Stopped {
			if n.LogState != "sealed" {
				return Evidence{}, errors.New("session recovery unresolved")
			}
			result.Completed = append(result.Completed, s)
		}
	}
	if requireStopped && len(result.Completed) == 0 {
		return Evidence{}, errors.New("no stopped session")
	}
	// Ordering is intentional: completion reads precede the full historical scan.
	if _, _, err := scanLog(ctx, r, &budget); err != nil {
		return Evidence{}, err
	}
	result.ObservedAt = now()
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	if !fresh(req.CapturedAt, result.ObservedAt, req.MaxAge) {
		return Evidence{}, errors.New("assessment expired")
	}
	result.OperationID = req.OperationID
	return result, nil
}
func list(ctx context.Context, r Reader, prefix string, budget *int) ([]listed, error) {
	return listEach(ctx, r, prefix, budget, nil)
}

// listed is one object exactly as the fresh listing named it. The ETag and Size
// are whatever that listing reported and are never carried over from an earlier
// pass; an unreported ETag is the empty string, which no cached record matches.
type listed struct {
	key, etag string
	size      int64
}

func listEach(ctx context.Context, r Reader, prefix string, budget *int, visit func(string) error) ([]listed, error) {
	var entries []listed
	seenTokens, seenKeys := map[string]bool{}, map[string]bool{}
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if *budget <= 0 {
			return nil, errors.New("listing budget exhausted")
		}
		*budget--
		page, err := r.List(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		if len(page.Keys) > 1000 || len(seenKeys)+len(page.Keys) > 100000 {
			return nil, errors.New("listing key budget exceeded")
		}
		// Metadata is positional, so a page that reports some of it must report
		// exactly as much of it as it has keys. A short or long vector could
		// otherwise attach one object's ETag to another object's key.
		if (len(page.ETags) != 0 && len(page.ETags) != len(page.Keys)) || (len(page.Sizes) != 0 && len(page.Sizes) != len(page.Keys)) {
			return nil, errors.New("listing metadata does not match its keys")
		}
		for i, key := range page.Keys {
			if !strings.HasPrefix(key, prefix) || seenKeys[key] {
				return nil, errors.New("invalid or duplicate listing key")
			}
			seenKeys[key] = true
			if visit != nil {
				if err := visit(key); err != nil {
					return nil, err
				}
				continue
			}
			entry := listed{key: key}
			if len(page.ETags) != 0 {
				entry.etag = page.ETags[i]
			}
			if len(page.Sizes) != 0 {
				if page.Sizes[i] < 0 {
					return nil, errors.New("invalid listing size")
				}
				entry.size = page.Sizes[i]
			}
			entries = append(entries, entry)
		}
		if page.Complete {
			if page.Next != "" {
				return nil, errors.New("ambiguous final page")
			}
			return entries, nil
		}
		if page.Next == "" {
			return nil, errors.New("incomplete listing")
		}
		if seenTokens[page.Next] {
			return nil, errors.New("repeated continuation")
		}
		seenTokens[page.Next] = true
		token = page.Next
	}
}

// readNodes lists nodes/ and produces a record for every key it names. The
// listing spends the caller's shared page budget. When want is non-negative the
// listing size is compared against it and mismatch is returned before any body
// is read, so an inventory that changed under us costs no object reads; pass -1
// and a nil mismatch to accept whatever the fleet currently publishes.
//
// The pinned runtime retains folded records as sealed tombstones rather than
// deleting them, so this listing names every invocation the fleet has ever had.
// A body is therefore read only when the listing cannot prove its content is
// unchanged since this process last parsed it. The rules are:
//
//   - The listing is always fresh from the primary bucket. A cached listing is
//     never used, and a listing that is incomplete, over budget, or internally
//     inconsistent fails before any record is reused, so reuse can never let a
//     partial listing pass as complete. Pages cost the page budget they always
//     did, whether or not their records are then re-read.
//   - A record is reused only on exact equality of the listed ETag with the ETag
//     of the body this process last parsed for that key, and on the listed size
//     matching the size recorded with it. A key the listing gives no ETag for is
//     always re-read.
//   - A Get whose returned ETag differs from the listed ETag means the object
//     changed between the list and the read: the pass fails closed and the next
//     reconcile starts from a fresh listing. A body whose length disagrees with
//     the size listed against a matching ETag fails the same way.
//   - Nothing is remembered unless the Get itself confirmed the listed ETag, so
//     a Reader that cannot report an ETag re-reads every record forever.
//   - Parse failures are never remembered; the next pass reads that body again.
//   - Keys the fresh listing no longer names are evicted, so a record that
//     disappears and later reappears is read again rather than resurrected.
//
// SSE-KMS objects carry a non-MD5 ETag, so an ETag is not a content digest
// there. It remains a version token that S3 changes on every rewrite of the
// object, which is the only property equality is relied on for here.
//
// Loss detection is unaffected: scanLog works on key names from its own fresh
// log/ listing and reads no bodies at all.
//
// On failure the records produced before the error are still returned: they are
// negative observations that Inventory callers retain. They are NEVER positive
// evidence, so every caller that assesses completion discards them.
func (a *Adapter) readNodes(ctx context.Context, r Reader, budget *int, want int, mismatch error) ([]Node, error) {
	var nodes []Node
	entries, err := list(ctx, r, "nodes/", budget)
	if err != nil {
		return nodes, err
	}
	if want >= 0 && len(entries) != want {
		return nodes, mismatch
	}
	a.retain(entries)
	verifier, _ := r.(ETagReader)
	for _, entry := range entries {
		if n, ok := a.reuse(entry); ok {
			nodes = append(nodes, n)
			continue
		}
		var data []byte
		var etag string
		if verifier != nil {
			data, etag, err = verifier.GetETag(ctx, entry.key)
		} else {
			data, err = r.Get(ctx, entry.key)
		}
		if err != nil {
			return nodes, err
		}
		confirmed := entry.etag != "" && etag == entry.etag
		if entry.etag != "" && etag != "" && !confirmed {
			return nodes, errors.New("record changed between the listing and the read")
		}
		if confirmed && entry.size != 0 && entry.size != int64(len(data)) {
			return nodes, errors.New("listed size disagrees with the record body")
		}
		n, err := a.ParseNode(entry.key, data)
		if err != nil {
			return nodes, err
		}
		if confirmed {
			a.remember(entry, n)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// scanLog walks the complete log/ listing. It reports whether ANY peer-log
// object exists and fails closed on the first loss declaration; the loss key is
// returned alongside the error so partial results can retain the observation.
// The scan is deliberately not short-circuited on an ordinary log name, so a
// later loss stays distinguishable from a bucket with no peer-log history.
func scanLog(ctx context.Context, r Reader, budget *int) (bool, string, error) {
	var hasLog bool
	var loss string
	_, err := listEach(ctx, r, "log/", budget, func(key string) error {
		hasLog = true
		if strings.HasSuffix(key, ".loss.json") {
			loss = key
			return &LossError{Key: key}
		}
		return nil
	})
	return hasLog, loss, err
}

// LossError is sticky lifecycle evidence, even if the object later disappears.
type LossError struct{ Key string }

func (e *LossError) Error() string { return "possible loss declaration: " + e.Key }
