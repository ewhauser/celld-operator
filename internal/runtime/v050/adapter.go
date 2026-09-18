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
type Adapter struct{}

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
	Pressured, MemoryHeadroom                                           bool
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
	for k, p := range map[string]any{"sampled_ms": &sampled, "resident_cells": &s.ResidentCells, "rss_bytes": &s.RSSBytes, "in_use_bytes": &s.InUseBytes, "pressured": &s.Pressured, "memory_headroom": &s.MemoryHeadroom} {
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
type Page struct {
	Keys     []string
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
	if req.OperationID == "" || !req.InventoryComplete || len(req.Sessions) == 0 || req.PageBudget <= 0 || !fresh(req.CapturedAt, now(), req.MaxAge) {
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
	keys, err := list(ctx, r, "nodes/", &budget)
	if err != nil {
		return Evidence{}, err
	}
	if len(keys) != len(expected) {
		return Evidence{}, errors.New("inventory changed or missing nodes")
	}
	for _, key := range keys {
		data, err := r.Get(ctx, key)
		if err != nil {
			return Evidence{}, err
		}
		n, err := a.ParseNode(key, data)
		if err != nil {
			return Evidence{}, err
		}
		s, ok := expected[n.Name]
		if !ok || s.Generation != n.Generation {
			return Evidence{}, errors.New("unknown node or generation replacement")
		}
		if !s.Stopped && (now().UnixMilli() < 0 || n.ExpiresMS <= uint64(now().UnixMilli())) {
			return Evidence{}, errors.New("unresolved unavailable session")
		}
		if s.Stopped {
			if n.LogState != "sealed" || n.Epoch < s.Epoch {
				return Evidence{}, errors.New("session recovery unresolved")
			}
			result.Completed = append(result.Completed, s)
		}
	}
	if requireStopped && len(result.Completed) == 0 {
		return Evidence{}, errors.New("no stopped session")
	}
	// Ordering is intentional: completion reads precede the full historical scan.
	keys, err = list(ctx, r, "log/", &budget)
	if err != nil {
		return Evidence{}, err
	}
	for _, key := range keys {
		if strings.HasSuffix(key, ".loss.json") {
			return Evidence{}, &LossError{Key: key}
		}
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
func list(ctx context.Context, r Reader, prefix string, budget *int) ([]string, error) {
	var keys []string
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
		for _, key := range page.Keys {
			if !strings.HasPrefix(key, prefix) || seenKeys[key] {
				return nil, errors.New("invalid or duplicate listing key")
			}
			seenKeys[key] = true
			keys = append(keys, key)
		}
		if page.Complete {
			if page.Next != "" {
				return nil, errors.New("ambiguous final page")
			}
			return keys, nil
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

// LossError is sticky lifecycle evidence, even if the object later disappears.
type LossError struct{ Key string }

func (e *LossError) Error() string { return "possible loss declaration: " + e.Key }
