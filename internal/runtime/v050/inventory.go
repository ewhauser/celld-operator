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
	keys, err := list(ctx, r, "nodes/", &budget)
	if err != nil {
		return out, err
	}
	for _, key := range keys {
		data, err := r.Get(ctx, key)
		if err != nil {
			return out, err
		}
		n, err := a.ParseNode(key, data)
		if err != nil {
			return out, err
		}
		out.Nodes = append(out.Nodes, n)
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
