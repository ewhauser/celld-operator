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
	Keys       []string                   `json:"log_keys"`
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
	return c, nil
}
func (c capture) Get(_ context.Context, key string) ([]byte, error) {
	data, ok := c.Nodes[key]
	if !ok {
		return nil, errors.New("missing node")
	}
	return data, nil
}
func (c capture) List(_ context.Context, prefix, _ string) (v050.Page, error) {
	if prefix == "log/" {
		return v050.Page{Keys: c.Keys, Complete: c.Complete}, nil
	}
	if prefix != "nodes/" {
		return v050.Page{}, errors.New("unapproved prefix")
	}
	keys := make([]string, 0, len(c.Nodes))
	for key := range c.Nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return v050.Page{Keys: keys, Complete: c.Complete}, nil
}
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
		stoppedSet[node] = true
	}
	req := v050.Request{OperationID: "offline-replay", InventoryComplete: before.Complete, CapturedAt: time.UnixMilli(before.ObservedMS), MaxAge: *age, PageBudget: 10}
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
