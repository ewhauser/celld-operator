// celld-qualify replays captured evidence. It never authorizes a live removal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

type capture struct {
	ObservedMS int64                      `json:"observed_unix_ms"`
	Complete   bool                       `json:"complete"`
	Nodes      map[string]json.RawMessage `json:"nodes"`
	// ETags records the ETag the collector saw on each captured body. Captures
	// taken before the field existed carry none, and a key without one is read
	// from the capture on every pass, which is what replay has always done.
	ETags map[string]string `json:"node_etags"`
	Keys  []string          `json:"log_keys"`
}

func read(path string) (capture, error) {
	var c capture
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if !c.Complete || c.ObservedMS <= 0 || c.Nodes == nil || c.Keys == nil {
		return c, errors.New("incomplete capture")
	}
	// Listing replays a sorted, duplicate-free key space: the continuation token
	// is the last key of a page, so ordering is part of the capture's contract.
	sort.Strings(c.Keys)
	for i := 1; i < len(c.Keys); i++ {
		if c.Keys[i] == c.Keys[i-1] {
			return c, errors.New("duplicate log key in capture")
		}
	}
	// An ETag for a body the capture does not hold describes nothing, and would
	// let a replay claim a record it cannot show. Reject the capture instead.
	for key := range c.ETags {
		if _, ok := c.Nodes[key]; !ok {
			return c, errors.New("capture ETag names an absent record")
		}
	}
	return c, nil
}
func (c capture) Get(ctx context.Context, key string) ([]byte, error) {
	data, _, err := c.GetETag(ctx, key)
	return data, err
}

// GetETag replays the ETag captured with this body, so the adapter's listing and
// read agree for a modern capture and disagree for none: an old capture reports
// the empty string everywhere and is re-read on every pass.
func (c capture) GetETag(_ context.Context, key string) ([]byte, string, error) {
	data, ok := c.Nodes[key]
	if !ok {
		return nil, "", errors.New("missing node")
	}
	return data, c.ETags[key], nil
}
func (c capture) List(_ context.Context, prefix, continuation string) (v050.Page, error) {
	if prefix == "log/" {
		return page(c.Keys, continuation)
	}
	if prefix != "nodes/" {
		return v050.Page{}, errors.New("unapproved prefix")
	}
	keys := make([]string, 0, len(c.Nodes))
	for key := range c.Nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out, err := page(keys, continuation)
	if err != nil {
		return v050.Page{}, err
	}
	for _, key := range out.Keys {
		out.ETags = append(out.ETags, c.ETags[key])
		out.Sizes = append(out.Sizes, int64(len(c.Nodes[key])))
	}
	return out, nil
}

// pageSize matches the real S3 reader's MaxKeys and the adapter's per-page key
// budget: a single-page capture of a real fleet is rejected outright.
const pageSize = 1000

// page slices a sorted, duplicate-free key space the way the S3 reader pages a
// bucket: at most pageSize keys, a continuation token naming the last key
// returned (so it never repeats the token just consumed), and a final page that
// is explicitly complete and carries no token.
func page(keys []string, continuation string) (v050.Page, error) {
	start := 0
	if continuation != "" {
		start = sort.SearchStrings(keys, continuation)
		if start == len(keys) || keys[start] != continuation {
			return v050.Page{}, errors.New("unknown continuation token")
		}
		start++
	}
	if end := start + pageSize; end < len(keys) {
		return v050.Page{Keys: keys[start:end:end], Next: keys[end-1]}, nil
	}
	return v050.Page{Keys: keys[start:len(keys):len(keys)], Complete: true}, nil
}

// budget counts the pages a key space of n keys needs, so a large capture is not
// rejected as an exhausted listing budget.
func budget(n int) int { return n/pageSize + 1 }
func run() error {
	beforePath := flag.String("before", "", "pre-disruption metadata capture")
	afterPath := flag.String("after", "", "post-stop metadata capture")
	stopped := flag.String("stopped", "", "comma-separated confirmed stopped node IDs (include prior stopped sessions)")
	image := flag.String("image", v050.Image, "verified runtime image digest")
	age := flag.Duration("max-age", 2*time.Minute, "maximum captured assessment duration")
	flag.Parse()
	adapter, err := v050.New(*image)
	if err != nil {
		return err
	}
	before, err := read(*beforePath)
	if err != nil {
		return err
	}
	after, err := read(*afterPath)
	if err != nil {
		return err
	}
	stoppedSet := map[string]bool{}
	for node := range strings.SplitSeq(*stopped, ",") {
		if node = strings.TrimSpace(node); node != "" {
			stoppedSet[node] = true
		}
	}
	if len(stoppedSet) == 0 {
		return errors.New("-stopped requires at least one confirmed stopped node ID")
	}
	req := v050.Request{OperationID: "offline-replay", InventoryComplete: before.Complete, CapturedAt: time.UnixMilli(before.ObservedMS), MaxAge: *age, PageBudget: budget(len(after.Keys)) + budget(len(after.Nodes))}
	for key, data := range before.Nodes {
		node, err := adapter.ParseNode(key, data)
		if err != nil {
			return err
		}
		req.Sessions = append(req.Sessions, v050.Session{Node: node.Name, Generation: node.Generation, Epoch: node.Epoch, Stopped: stoppedSet[node.Name]})
		delete(stoppedSet, node.Name)
	}
	if len(stoppedSet) != 0 {
		return errors.New("unknown or missing stopped identity")
	}
	result, err := adapter.Assess(context.Background(), after, req, func() time.Time { return time.UnixMilli(after.ObservedMS) })
	if err != nil {
		return fmt.Errorf("BLOCKED (offline replay): %w", err)
	}
	sort.Slice(result.Completed, func(i, j int) bool { return result.Completed[i].Node < result.Completed[j].Node })
	return json.NewEncoder(os.Stdout).Encode(struct {
		Scope    string
		Evidence v050.Evidence
	}{"offline candidate evidence; not live removal authorization", result})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
