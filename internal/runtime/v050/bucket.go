package v050

import (
	"context"
	"errors"
	"time"
)

// BucketObservation is preflight metadata, never a recovery or process-fencing
// certificate. Bucket durability comes from the pinned runtime's acknowledgement
// rule and verified immutable configuration, not from absence of log objects.
type BucketObservation struct {
	ObservedAt time.Time
}

// InspectBucket checks the narrow no-peer-log case. It deliberately does not
// reuse Assess: that method requires sealed recovery records for stopped peers.
// Unknown writers, unresolved historical sessions, and ANY peer-log object make
// this case inapplicable, even when an object is sealed or not a loss declaration.
func (a *Adapter) InspectBucket(ctx context.Context, r Reader, req Request, now func() time.Time) (BucketObservation, error) {
	if req.OperationID == "" || !req.InventoryComplete || len(req.Sessions) == 0 || req.PageBudget <= 0 || !fresh(req.CapturedAt, now(), req.MaxAge) {
		return BucketObservation{}, errors.New("incomplete Bucket preflight")
	}
	ctx, cancel := context.WithTimeout(ctx, req.MaxAge)
	defer cancel()
	expected := map[string]string{}
	for _, s := range req.Sessions {
		if !validID(s.Node) || !validID(s.Generation) || s.Epoch != 0 || s.Stopped || expected[s.Node] != "" {
			return BucketObservation{}, errors.New("bucket history requires independent recovery assessment")
		}
		expected[s.Node] = s.Generation
	}
	budget := req.PageBudget
	keys, err := list(ctx, r, "nodes/", &budget)
	if err != nil {
		return BucketObservation{}, err
	}
	if len(keys) != len(expected) {
		return BucketObservation{}, errors.New("bucket writer inventory changed or missing")
	}
	for _, key := range keys {
		data, err := r.Get(ctx, key)
		if err != nil {
			return BucketObservation{}, err
		}
		n, err := a.ParseNode(key, data)
		if err != nil {
			return BucketObservation{}, err
		}
		if expected[n.Name] != n.Generation || n.Epoch != 0 || n.LogState != "" {
			return BucketObservation{}, errors.New("bucket generation changed or peer-log session present")
		}
		if now().UnixMilli() < 0 || n.ExpiresMS <= uint64(now().UnixMilli()) || !fresh(time.UnixMilli(n.SampledMS), now(), req.MaxAge) {
			return BucketObservation{}, errors.New("bucket writer observation expired")
		}
	}
	// scanLog completes even after an ordinary log name, so a later loss remains
	// distinguishable and can acquire the controller's durable loss fence.
	hasLog, _, err := scanLog(ctx, r, &budget)
	if err != nil {
		return BucketObservation{}, err
	}
	if hasLog {
		return BucketObservation{}, errors.New("bucket contains peer-log history; no-log case inapplicable")
	}
	if err := ctx.Err(); err != nil {
		return BucketObservation{}, err
	}
	at := now()
	if !fresh(req.CapturedAt, at, req.MaxAge) {
		return BucketObservation{}, errors.New("bucket preflight expired")
	}
	return BucketObservation{ObservedAt: at}, nil
}

// BucketExpiryInvalidatedError revokes prior expiry observations for the
// expected generation after contrary live-lease or replacement evidence.
type BucketExpiryInvalidatedError struct{ Node, Generation, Reason string }

func (e *BucketExpiryInvalidatedError) Error() string {
	return e.Reason
}

// BucketMember describes an operator-configured Bucket generation. Retired means
// absent from desired Kubernetes membership, NOT physically stopped.
type BucketMember struct {
	Node, Generation string
	// SupersededBy is an exact positively observed admitted successor generation.
	// This is logical membership evidence, never physical process termination.
	SupersededBy string
	Retired      bool
	// Resolved records durably observed positive expiry; survivor settling is
	// independently enforced by the controller.
	Resolved bool
}

// InspectBucketMembership supports repeated removals without erasing historical
// sessions. New retired generations need positively read expired leases. Only
// records with durably observed expiry may subsequently disappear through GC.
func (a *Adapter) InspectBucketMembership(ctx context.Context, r Reader, members []BucketMember, now func() time.Time) (BucketObservation, error) {
	started := now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	expected := map[string]BucketMember{}
	generations := map[string]map[string]BucketMember{}
	for _, member := range members {
		if !validID(member.Node) || !validID(member.Generation) {
			return BucketObservation{}, errors.New("invalid bucket membership")
		}
		if generations[member.Node] == nil {
			generations[member.Node] = map[string]BucketMember{}
		}
		if _, exists := generations[member.Node][member.Generation]; exists {
			return BucketObservation{}, errors.New("duplicate bucket generation")
		}
		generations[member.Node][member.Generation] = member
		if member.SupersededBy == "" {
			if expected[member.Node].Node != "" {
				return BucketObservation{}, errors.New("ambiguous bucket membership")
			}
			expected[member.Node] = member
		}
	}
	for _, member := range members {
		if member.SupersededBy == "" {
			continue
		}
		if !member.Retired {
			return BucketObservation{}, errors.New("current bucket generation cannot be superseded")
		}
		next := member
		for hops := 0; next.SupersededBy != ""; hops++ {
			if hops >= len(members) {
				return BucketObservation{}, errors.New("cyclic bucket succession")
			}
			var ok bool
			next, ok = generations[member.Node][next.SupersededBy]
			if !ok {
				return BucketObservation{}, errors.New("bucket successor missing")
			}
		}
		if expected[member.Node].Generation != next.Generation {
			return BucketObservation{}, errors.New("bucket successor authority ambiguous")
		}
	}
	if len(expected) == 0 {
		return BucketObservation{}, errors.New("empty bucket membership")
	}
	budget := 1000
	keys, err := list(ctx, r, "nodes/", &budget)
	if err != nil {
		return BucketObservation{}, err
	}
	seen := map[string]bool{}
	for _, key := range keys {
		body, err := r.Get(ctx, key)
		if err != nil {
			return BucketObservation{}, err
		}
		node, err := a.ParseNode(key, body)
		if err != nil {
			return BucketObservation{}, err
		}
		seen[node.Name] = true
		s, ok := expected[node.Name]
		if ok && node.Generation != s.Generation {
			return BucketObservation{}, &BucketExpiryInvalidatedError{Node: s.Node, Generation: s.Generation, Reason: "bucket generation replaced"}
		}
		if !ok || node.Epoch != 0 || node.LogState != "" {
			return BucketObservation{}, errors.New("unknown bucket generation or peer recovery obligation")
		}
		if now().UnixMilli() < 0 {
			return BucketObservation{}, errors.New("invalid observation clock")
		}
		live := node.ExpiresMS > uint64(now().UnixMilli())
		if s.Retired && live {
			return BucketObservation{}, &BucketExpiryInvalidatedError{Node: node.Name, Generation: node.Generation, Reason: "retired bucket process still has a live lease"}
		}
		if !s.Retired && (!live || !fresh(time.UnixMilli(node.SampledMS), now(), 5*time.Second)) {
			return BucketObservation{}, errors.New("current bucket lease or sample unavailable")
		}
	}
	for node, member := range expected {
		if !seen[node] && (!member.Retired || !member.Resolved) {
			return BucketObservation{}, errors.New("unresolved bucket writer record missing")
		}
	}
	hasLog, _, err := scanLog(ctx, r, &budget)
	if err != nil {
		return BucketObservation{}, err
	}
	if hasLog {
		return BucketObservation{}, errors.New("bucket peer-log history unresolved")
	}
	if err := ctx.Err(); err != nil {
		return BucketObservation{}, err
	}
	at := now()
	if !fresh(started, at, 5*time.Second) {
		return BucketObservation{}, errors.New("bucket assessment expired")
	}
	return BucketObservation{ObservedAt: at}, nil
}
