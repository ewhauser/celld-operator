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
