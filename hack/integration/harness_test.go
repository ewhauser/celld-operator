package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInterruptedCommandCanBeCollectedBeforeCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := harness{ctx: ctx}
	// Concurrent node pulls collect errors in their parent. A panic in a worker
	// would bypass the harness's deferred, cluster-scoped cleanup entirely.
	_, err := h.try(command{args: []string{"command-must-not-start"}, timeout: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context cancellation", err)
	}
}

func TestStoredResponseIsBoundToRequestedID(t *testing.T) {
	for _, value := range []bool{false, true} {
		if got := stored(object{"id": "expected", "stored": value}, "expected"); got != value {
			t.Fatalf("stored = %v; want %v", got, value)
		}
	}
	for name, response := range map[string]object{
		"foreign ID":        {"id": "foreign", "stored": true},
		"missing ID":        {"stored": true},
		"missing stored":    {"id": "expected"},
		"nonboolean stored": {"id": "expected", "stored": "true"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("malformed or foreign response was accepted")
				}
			}()
			stored(response, "expected")
		})
	}
}
