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
	if req.incomplete(now) {
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
	nodes, err := a.readNodes(ctx, r, &budget, len(expected), errors.New("bucket writer inventory changed or missing"))
	if err != nil {
		return BucketObservation{}, err
	}
	for _, n := range nodes {
		if expected[n.Name] != n.Generation || n.Epoch != 0 || n.LogState != "" {
			return BucketObservation{}, errors.New("bucket generation changed or peer-log session present")
		}
		// One clock read per writer: the sign check, the lease comparison and the
		// sample freshness test must all judge the same instant.
		at := now()
		if at.UnixMilli() < 0 || n.ExpiresMS <= uint64(at.UnixMilli()) || !fresh(time.UnixMilli(n.SampledMS), at, req.MaxAge) {
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

// BucketEvidenceLostError reports an admitted generation whose record left the
// store before any assessment positively read its lease expired. Absence never
// resolves a retirement, and no elapsed time or prior live lease reconstructs
// the proof, so this blocker is terminal for the operation rather than a state
// the controller can wait out.
type BucketEvidenceLostError struct{ Node, Generation string }

func (e *BucketEvidenceLostError) Error() string {
	return "unresolved bucket writer record missing"
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

// RetirementObservation names one admitted generation whose node record was
// positively read while its lease had already elapsed.
type RetirementObservation struct{ Node, Generation string }

// ObserveBucketRetirement reads the named generations' node records and reports
// only those positively read with an expired lease. The pinned dead_node_gc
// deletes a no-log record about a second after the lease elapses, so a caller
// that must gather slower or more failure-prone evidence in the same pass can
// lose that window; this lets it take the reading first.
//
// It narrows InspectBucketMembership, never replaces it. A record still has to
// match the expected generation exactly, carry no epoch and no peer-log state,
// and show a lease that has already elapsed against a single clock read. A
// record that is absent, unreadable or still live yields nothing: absence is
// never expiry, and this reports no negative finding either, so it can neither
// manufacture nor revoke authority. Membership consistency, survivor health and
// peer-log obligations are deliberately not judged here; they remain
// InspectBucketMembership's job on this pass and on every later one, and an
// observation recorded here only ever lets that function tolerate the exact
// record's later disappearance.
func (a *Adapter) ObserveBucketRetirement(ctx context.Context, r Reader, members []BucketMember, now func() time.Time) ([]RetirementObservation, error) {
	if len(members) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	expected := map[string]string{}
	for _, member := range members {
		if !validID(member.Node) || !validID(member.Generation) || member.SupersededBy != "" {
			return nil, errors.New("invalid bucket retirement probe")
		}
		if _, exists := expected[member.Node]; exists {
			return nil, errors.New("duplicate bucket retirement probe")
		}
		expected[member.Node] = member.Generation
	}
	budget := 1000
	nodes, err := a.readNodes(ctx, r, &budget, -1, nil)
	if err != nil {
		return nil, err
	}
	var observed []RetirementObservation
	for _, node := range nodes {
		generation, ok := expected[node.Name]
		if !ok || node.Generation != generation || node.Epoch != 0 || node.LogState != "" {
			continue
		}
		// One clock read per record, as the full assessment does: the sign check
		// and the lease comparison must judge the same instant.
		at := now()
		if at.UnixMilli() < 0 {
			return nil, errors.New("invalid observation clock")
		}
		if node.ExpiresMS > uint64(at.UnixMilli()) {
			continue
		}
		observed = append(observed, RetirementObservation{Node: node.Name, Generation: node.Generation})
	}
	return observed, nil
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
	nodes, err := a.readNodes(ctx, r, &budget, -1, nil)
	if err != nil {
		return BucketObservation{}, err
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		seen[node.Name] = true
		s, ok := expected[node.Name]
		if ok && node.Generation != s.Generation {
			return BucketObservation{}, &BucketExpiryInvalidatedError{Node: s.Node, Generation: s.Generation, Reason: "bucket generation replaced"}
		}
		if !ok || node.Epoch != 0 || node.LogState != "" {
			return BucketObservation{}, errors.New("unknown bucket generation or peer recovery obligation")
		}
		// One clock read per record: liveness and sample freshness must not be
		// judged against two different instants.
		at := now()
		if at.UnixMilli() < 0 {
			return BucketObservation{}, errors.New("invalid observation clock")
		}
		live := node.ExpiresMS > uint64(at.UnixMilli())
		if s.Retired && live {
			return BucketObservation{}, &BucketExpiryInvalidatedError{Node: node.Name, Generation: node.Generation, Reason: "retired bucket process still has a live lease"}
		}
		if !s.Retired && (!live || !fresh(time.UnixMilli(node.SampledMS), at, 5*time.Second)) {
			return BucketObservation{}, errors.New("current bucket lease or sample unavailable")
		}
	}
	// Name one writer deterministically rather than whichever the map yields, so
	// the reported condition is stable across passes.
	var lost *BucketEvidenceLostError
	for node, member := range expected {
		if seen[node] || (member.Retired && member.Resolved) {
			continue
		}
		if lost == nil || node < lost.Node {
			lost = &BucketEvidenceLostError{Node: node, Generation: member.Generation}
		}
	}
	if lost != nil {
		return BucketObservation{}, lost
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
