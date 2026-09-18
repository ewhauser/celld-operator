package v050

import (
	"context"
	"errors"
	"strings"
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
	// Complete the scan even after an ordinary log name, so a later loss remains
	// distinguishable and can acquire the controller's durable loss fence.
	hasLog := false
	_, err = listEach(ctx, r, "log/", &budget, func(key string) error {
		hasLog = true
		if strings.HasSuffix(key, ".loss.json") {
			return &LossError{Key: key}
		}
		return nil
	})
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
