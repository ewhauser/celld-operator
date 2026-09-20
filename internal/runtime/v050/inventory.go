package v050

import (
	"context"
	"errors"
	"time"
)

// Inventory reads node records before the complete loss scan. Partial results
// retain negative observations but MUST NOT be used as positive evidence.
type Inventory struct {
	Nodes      []Node
	Loss       string
	ObservedAt time.Time
}

func (a *Adapter) Inventory(ctx context.Context, r Reader, now func() time.Time) (Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := now()
	out := Inventory{}
	budget := 1000
	// Records read before a failure are kept: see readNodes.
	nodes, err := a.readNodes(ctx, r, &budget, -1, nil)
	out.Nodes = nodes
	if err != nil {
		return out, err
	}
	_, loss, err := scanLog(ctx, r, &budget)
	out.Loss = loss
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.ObservedAt = now()
	if !fresh(started, out.ObservedAt, 5*time.Second) {
		return out, errors.New("inventory expired")
	}
	return out, nil
}
