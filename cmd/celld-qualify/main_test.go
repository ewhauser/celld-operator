package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

// writeCapture builds a capture whose only node is the sealed fixture and whose
// log key space is logKeys objects, the shape a real fleet's bucket has.
func writeCapture(t *testing.T, path string, observed int64, logKeys int) {
	t.Helper()
	node, err := os.ReadFile("testdata/node-sealed.json")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, logKeys)
	for i := range logKeys {
		keys = append(keys, fmt.Sprintf("log/%06d.log", i))
	}
	data, err := json.Marshal(map[string]any{
		"observed_unix_ms": observed,
		"complete":         true,
		"nodes":            map[string]json.RawMessage{"nodes/a.json": node},
		"log_keys":         keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// qualify runs the command with its own flag set and a discarded stdout, so the
// global command line stays usable across cases.
func qualify(t *testing.T, args ...string) error {
	t.Helper()
	oldArgs, oldFlags, oldStdout := os.Args, flag.CommandLine, os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Args, flag.CommandLine, os.Stdout = oldArgs, oldFlags, oldStdout
		if err := devNull.Close(); err != nil {
			t.Error(err)
		}
	})
	flag.CommandLine = flag.NewFlagSet("celld-qualify", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = append([]string{"celld-qualify"}, args...)
	os.Stdout = devNull
	return run()
}

// captures writes a fresh before/after pair and returns the replay arguments.
func captures(t *testing.T, logKeys int) []string {
	t.Helper()
	dir := t.TempDir()
	before, after := filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json")
	now := time.Now().UnixMilli()
	writeCapture(t, before, now-1000, logKeys)
	writeCapture(t, after, now, logKeys)
	return []string{"-before", before, "-after", after}
}

// A capture larger than one page must replay: the adapter rejects any listing
// page over 1000 keys, so an unpaginated fake could not describe a real fleet.
func TestReplayPaginatesLargeCapture(t *testing.T) {
	for _, logKeys := range []int{0, 999, 1000, 1001, 2500} {
		t.Run(fmt.Sprint(logKeys), func(t *testing.T) {
			if err := qualify(t, append(captures(t, logKeys), "-stopped", "a")...); err != nil {
				t.Fatalf("%d log keys: %v", logKeys, err)
			}
		})
	}
}

// page must satisfy every continuation rule the adapter's listEach enforces:
// bounded pages, prefixed and non-duplicate keys, non-repeating tokens and an
// unambiguous final page.
func TestPageContinuation(t *testing.T) {
	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = fmt.Sprintf("log/%06d.log", i)
	}
	var (
		seen      []string
		token     string
		tokens    = map[string]bool{}
		unique    = map[string]bool{}
		pageCount int
	)
	for {
		p, err := page(keys, token)
		if err != nil {
			t.Fatal(err)
		}
		pageCount++
		if len(p.Keys) > pageSize {
			t.Fatalf("page of %d keys exceeds the %d key budget", len(p.Keys), pageSize)
		}
		for _, key := range p.Keys {
			if !strings.HasPrefix(key, "log/") || unique[key] {
				t.Fatalf("invalid or duplicate key %q", key)
			}
			unique[key] = true
			seen = append(seen, key)
		}
		if p.Complete {
			if p.Next != "" {
				t.Fatal("final page carries a continuation token")
			}
			break
		}
		if p.Next == "" || p.Next == token || tokens[p.Next] {
			t.Fatalf("unusable continuation token %q after %q", p.Next, token)
		}
		tokens[p.Next] = true
		token = p.Next
		if pageCount > len(keys) {
			t.Fatal("listing does not terminate")
		}
	}
	if pageCount != 3 {
		t.Fatalf("2500 keys took %d pages, want 3", pageCount)
	}
	if len(seen) != len(keys) || seen[0] != keys[0] || seen[len(seen)-1] != keys[len(keys)-1] {
		t.Fatalf("listing returned %d keys, want the whole %d key space in order", len(seen), len(keys))
	}
	if _, err := page(keys, "log/nonexistent.log"); err == nil {
		t.Fatal("accepted an unknown continuation token")
	}
	if _, err := page(nil, ""); err != nil {
		t.Fatal(err)
	}
}

// The reported page budget must cover the pages the capture actually needs.
func TestBudget(t *testing.T) {
	for keys, want := range map[int]int{0: 1, 1: 1, 999: 1, 1000: 2, 1001: 2, 2500: 3} {
		if got := budget(keys); got != want {
			t.Fatalf("budget(%d) = %d, want %d", keys, got, want)
		}
	}
}

// An omitted or empty -stopped must say so, not report an unknown identity.
func TestStoppedRequired(t *testing.T) {
	args := captures(t, 4)
	for name, stopped := range map[string][]string{
		"omitted": nil,
		"empty":   {"-stopped", ""},
		"commas":  {"-stopped", " , ,"},
	} {
		t.Run(name, func(t *testing.T) {
			err := qualify(t, append(append([]string{}, args...), stopped...)...)
			if err == nil {
				t.Fatal("replayed without a stopped identity")
			}
			if !strings.Contains(err.Error(), "-stopped requires at least one confirmed stopped node ID") {
				t.Fatalf("unclear error: %v", err)
			}
		})
	}
}

// Empty entries are skipped, but a genuinely unknown identity still blocks.
func TestUnknownStoppedIdentity(t *testing.T) {
	err := qualify(t, append(captures(t, 4), "-stopped", "a,,b")...)
	if err == nil || !strings.Contains(err.Error(), "unknown or missing stopped identity") {
		t.Fatalf("got %v, want an unknown stopped identity", err)
	}
}

// The capture is a Reader the adapter can drive; keep that contract explicit.
var _ v050.Reader = capture{}
