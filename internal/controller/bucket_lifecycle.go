package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A retired session has left observed membership; physical liveness is unknown.
// Only sessions admitted with the pinned immutable Bucket configuration enter
// this ledger. Never turn an unknown historical writer into a retired session.
type bucketSession struct {
	Node, Generation, Container, Host, IP string
	SupersededBy                          string
	Retired                               bool
	ExpiryObserved                        bool
	ExpiryInvalidated                     bool
}

// resolvedBucketSession reports whether a session carries positive expiry
// authority that no later contrary lease observation has revoked. Only such a
// session may let the runtime adapter tolerate a garbage-collected record.
// Retirement alone is membership state, never expiry proof.
func resolvedBucketSession(s bucketSession) bool {
	return s.ExpiryObserved && !s.ExpiryInvalidated
}

// invalidateBucketExpiry durably revokes positive expiry proof for the exact
// generation named by a typed renewed-lease or generation-replacement
// observation, across every record set it is given. Sibling writers keep their
// own authority: contrary lease evidence about one generation says nothing
// about another, and blanket invalidation would un-resolve writers whose
// records the runtime may already have garbage-collected, wedging the
// operation on "unresolved bucket writer record missing". It reports whether
// any record changed.
func invalidateBucketExpiry(records [][]bucketSession, e *v050.BucketExpiryInvalidatedError) bool {
	changed := false
	for _, sessions := range records {
		for i := range sessions {
			s := &sessions[i]
			if s.Node != e.Node || s.Generation != e.Generation {
				continue
			}
			if s.ExpiryObserved || !s.ExpiryInvalidated {
				changed = true
			}
			s.ExpiryObserved = false
			s.ExpiryInvalidated = true
		}
	}
	return changed
}

// bucketAssessmentScope selects how much of the survivor capacity evidence an
// assessment demands on top of the membership, lease and loss checks that every
// scope performs.
type bucketAssessmentScope uint8

const (
	// bucketScopeCapacity is the full assessment used by contraction and restart:
	// survivors must also be healthy and, once the action is issued, the fleet
	// must still be under low demand with fresh Metrics Server samples.
	bucketScopeCapacity bucketAssessmentScope = iota
	// bucketScopeAdmission only records an observed invocation, which has no
	// replica effect, so it requires neither low demand nor Metrics Server.
	bucketScopeAdmission
	// bucketScopeShutdown authorizes RetainData deletion. Every member is going
	// away, so there is no projected survivor to keep healthy or under demand;
	// requiring a Collector here would block deletion on clusters without
	// Metrics Server or on fleets that are simply busy.
	bucketScopeShutdown
)

// needsCapacity reports whether the scope demands survivor capacity evidence.
func (s bucketAssessmentScope) needsCapacity() bool { return s == bucketScopeCapacity }

func (r *Reconciler) bucketAssessment(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, count int32, issued bool) ([]bucketSession, time.Time, error) {
	return r.bucketAssessmentMode(ctx, f, j, count, issued, bucketScopeCapacity)
}

func (r *Reconciler) bucketAssessmentMode(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, count int32, issued bool, scope bucketAssessmentScope) ([]bucketSession, time.Time, error) {
	view := *j
	op := *j.Operation
	op.From = count
	view.Operation = &op
	candidates, err := r.Evidence.bucketCandidates(ctx, f, &view, r.Options)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err := validateBucketPlacement(f, candidates, !issued && scope.needsCapacity()); err != nil {
		return nil, time.Time{}, err
	}
	sessions := slices.Clone(j.BucketHistory)
	for _, s := range j.Operation.BucketCandidates {
		index := slices.IndexFunc(sessions, func(old bucketSession) bool { return old.Node == s.Node && old.Generation == s.Generation })
		if index < 0 {
			sessions = append(sessions, s)
		} else {
			sessions[index] = s
		}
	}
	for uid, c := range candidates {
		var observed *RuntimeSession
		for i := range j.Inventory.Sessions {
			s := &j.Inventory.Sessions[i]
			if s.Node == string(uid) && s.Current && s.Container == c.Container && s.Epoch == 0 {
				if observed != nil {
					return nil, time.Time{}, errors.New("multiple current bucket generations")
				}
				observed = s
			}
		}
		if observed == nil {
			return nil, time.Time{}, errors.New("current bucket association unavailable")
		}
		index := slices.IndexFunc(sessions, func(s bucketSession) bool { return s.Node == string(uid) && s.SupersededBy == "" })
		current := bucketSession{Node: string(uid), Generation: observed.Generation, Container: c.Container, Host: c.Host, IP: c.IP}
		if index < 0 {
			sessions = append(sessions, current)
		} else {
			old := sessions[index]
			if old.Retired || old.Host != current.Host || old.IP != current.IP {
				return nil, time.Time{}, errors.New("bucket retired member or changed host returned")
			}
			if old.Generation != current.Generation {
				// Only an already admitted Bucket invocation may be superseded.
				// A new container and fresh matching successor lease are required;
				// adapter verification below positively reads that exact successor.
				if old.Container == current.Container || slices.ContainsFunc(sessions, func(s bucketSession) bool { return s.Node == current.Node && s.Generation == current.Generation }) {
					return nil, time.Time{}, errors.New("bucket successor reused a previous invocation")
				}
				sessions[index].SupersededBy = current.Generation
				sessions[index].Retired = true
				sessions = append(sessions, current)
			} else if old.Container != current.Container {
				return nil, time.Time{}, errors.New("bucket container changed without successor generation")
			}
		}
	}
	// All observed history must be accounted for. Replaced generations are not
	// automatically resolved merely because a successor has a valid lease.
	for _, s := range j.Inventory.Sessions {
		if !slices.ContainsFunc(sessions, func(known bucketSession) bool {
			return known.Node == s.Node && known.Generation == s.Generation && (known.Container == s.Container || s.Container == "") && s.Epoch == 0
		}) {
			return nil, time.Time{}, errors.New("unadmitted bucket history")
		}
	}
	var members []v050.BucketMember
	for i := range sessions {
		s := &sessions[i]
		_, present := candidates[types.UID(s.Node)]
		if !present && !s.Retired && !issued && j.Operation.Phase != "Blocked" {
			return nil, time.Time{}, errors.New("bucket membership changed before issue")
		}
		s.Retired = !present || s.SupersededBy != ""
		members = append(members, v050.BucketMember{Node: s.Node, Generation: s.Generation, SupersededBy: s.SupersededBy, Retired: s.Retired, Resolved: slices.ContainsFunc(j.BucketHistory, func(old bucketSession) bool {
			return old.Node == s.Node && old.Generation == s.Generation && old.Retired && resolvedBucketSession(old)
		}) || (issued && j.Operation.Phase == "Recovering" && slices.ContainsFunc(j.Operation.BucketCandidates, func(old bucketSession) bool {
			// Recovering candidates record ExpiryObserved only after a successful
			// issued assessment positively reads their expired lease. Persist proof
			// before settling so runtime GC cannot erase an already observed fact.
			return old.Node == s.Node && old.Generation == s.Generation && old.Retired && resolvedBucketSession(old)
		}))})
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return nil, time.Time{}, err
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return nil, time.Time{}, err
	}
	evidence, err := adapter.InspectBucketMembership(ctx, reader, members, r.capacityNow)
	if err != nil {
		return nil, time.Time{}, err
	}
	var observation capacity.Observation
	var maxAge time.Duration
	if scope.needsCapacity() {
		policy := f.DeepCopy()
		if policy.Spec.Capacity == nil {
			policy.Spec.Capacity = &fleet.CapacityPolicy{}
			policy.Spec.Capacity.Default()
		}
		if r.Collector == nil {
			return nil, time.Time{}, errors.New("bucket survivor collector unavailable")
		}
		observation = r.Collector.Collect(ctx, policy)
		var ids []string
		for _, c := range candidates {
			ids = append(ids, c.Container)
		}
		if issued {
			if !capacity.LowDemand(*policy.Spec.Capacity, observation, count) || !bucketObservationIdentities(observation, ids) {
				return nil, time.Time{}, errors.New("bucket survivor health or membership uncertain")
			}
		} else {
			for _, id := range ids {
				if err := ValidateSurvivors(*policy.Spec.Capacity, observation, ids, id, r.capacityNow()); err != nil {
					return nil, time.Time{}, err
				}
			}
		}
		maxAge = capacity.Seconds(policy.Spec.Capacity.MaxAgeSeconds)
	}
	after, err := r.Evidence.bucketCandidates(ctx, f, &view, r.Options)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !equality.Semantic.DeepEqual(candidates, after) || evidence.ObservedAt.After(r.capacityNow()) || r.capacityNow().Sub(evidence.ObservedAt) > 5*time.Second || (scope.needsCapacity() && (observation.At.After(r.capacityNow()) || r.capacityNow().Sub(observation.At) > maxAge)) {
		return nil, time.Time{}, errors.New("bucket assessment expired or membership changed")
	}
	if issued {
		for i := range sessions {
			if sessions[i].Retired && sessions[i].SupersededBy == "" {
				sessions[i].ExpiryObserved = true
				sessions[i].ExpiryInvalidated = false
			}
		}
	}
	slices.SortFunc(sessions, func(a, b bucketSession) int {
		if a.Node < b.Node {
			return -1
		}
		if a.Node > b.Node {
			return 1
		}
		return strings.Compare(a.Generation, b.Generation)
	})
	return sessions, evidence.ObservedAt, nil
}

// Scheduling constraints do not control Deployment deletion order. Admit a
// strict removal only if every possible victim preserves both AZ coverage and
// maxSkew. Relaxed placement retains its zone allowlist without this guarantee.
func validateBucketPlacement(f *fleet.CelldFleet, candidates map[types.UID]bucketCandidate, removing bool) error {
	counts := map[string]int{}
	for _, zone := range f.Spec.Placement.Zones {
		counts[zone] = 0
	}
	hosts := map[string]bool{}
	for _, c := range candidates {
		if _, ok := counts[c.Zone]; !ok {
			return errors.New("bucket member outside configured AZ allowlist")
		}
		counts[c.Zone]++
		if f.Spec.Placement.Mode != "Relaxed" && (c.Hostname == "" || hosts[c.Hostname]) {
			return errors.New("strict bucket hostname separation unavailable")
		}
		hosts[c.Hostname] = true
	}
	if f.Spec.Placement.Mode == "Relaxed" {
		return nil
	}
	balanced := func() bool {
		lo, hi := len(candidates)+1, 0
		for _, n := range counts {
			lo = min(lo, n)
			hi = max(hi, n)
		}
		return lo > 0 && hi-lo <= 1
	}
	if !balanced() {
		return errors.New("strict bucket AZ coverage or skew unavailable")
	}
	if removing {
		for _, c := range candidates {
			if orderedBucket(f) {
				n, err := bucketOrdinal(f, c.Pod)
				if err != nil {
					return err
				}
				if n != len(candidates)-1 {
					continue
				}
			}
			counts[c.Zone]--
			ok := balanced()
			counts[c.Zone]++
			if !ok {
				return errors.New("not every Deployment victim preserves strict AZ coverage and skew")
			}
		}
	}
	return nil
}
func bucketObservationIdentities(o capacity.Observation, ids []string) bool {
	if len(o.Samples) != len(ids) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range o.Samples {
		if seen[s.Identity] || !slices.Contains(ids, s.Identity) {
			return false
		}
		seen[s.Identity] = true
	}
	return true
}

func (r *Reconciler) contractBucket(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	report := func(reason, message string) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, reason, message, 0, false)
		return result, true, err
	}
	save := func() (ctrl.Result, bool, error) {
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
	}
	fail := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		dirty := false
		if invalidated, ok := errors.AsType[*v050.BucketExpiryInvalidatedError](err); ok {
			if invalidateBucketExpiry([][]bucketSession{op.BucketCandidates, j.BucketHistory}, invalidated) {
				dirty = true
			}
		}
		if !op.SettledAt.IsZero() {
			op.SettledAt = time.Time{}
			dirty = true
		}
		if dirty {
			if err := r.saveJournal(ctx, res, j); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		return report("BucketRecoveryBlocked", err.Error())
	}
	issued := w.GetAnnotations()[operationKey] == op.ID && replicas(w) == op.To
	if j.Loss != "" {
		return report("PossibleDataLoss", j.Loss)
	}
	if !issued {
		if op.Phase != "Blocked" && op.Phase != "Intent" {
			return fail(errors.New("issued bucket authority no longer matches the workload"))
		}
		if op.Stalled {
			return report("OperationStalled", "Bucket operation deadline exceeded before replica issue")
		}
		if maintenanceFence(f) != "" {
			return report("MaintenancePaused", "Bucket removal remains unissued")
		}
		if op.Automatic && !r.Options.LocalTest {
			return report("BucketAutomaticUnqualified", "Automatic Bucket contraction awaits EKS/S3 release qualification; manual contraction uses the same safety checks")
		}
		if externalOwner(f) && !r.Options.LocalTest {
			return report("ExternalContractionUnqualified", "Contraction requested through /scale by an external writer awaits the same EKS/S3 release qualification as Automatic mode; additions proceed")
		}
		if !op.Automatic && f.Spec.Replicas >= op.From {
			return report("DesiredChanged", "Bucket removal intent retained until canceled or requested again")
		}
		if op.Automatic {
			if f.Spec.Capacity == nil || f.Spec.Capacity.Mode != "Automatic" || j.Capacity == nil || op.PolicyHash != j.Capacity.Config || op.ManualBaseline != f.Spec.Replicas || op.To < f.Spec.Capacity.MinReplicas {
				return report("CapacityChanged", "Automatic Bucket removal policy changed")
			}
		}
		sessions, assessedAt, err := r.bucketAssessment(ctx, f, j, op.From, false)
		if err != nil {
			return fail(err)
		}
		if op.Phase == "Blocked" {
			op.BucketCandidates = sessions
			op.Phase = "Intent"
			op.WorkloadVersion = w.GetResourceVersion()
			return save()
		}
		if !equality.Semantic.DeepEqual(sessions, op.BucketCandidates) {
			// The fresh complete assessment admitted an exact successor. Persist
			// its authority and revalidate again before the workload CAS.
			op.BucketCandidates = sessions
			op.WorkloadVersion = w.GetResourceVersion()
			return save()
		}
		if w.GetResourceVersion() != op.WorkloadVersion {
			op.WorkloadVersion = w.GetResourceVersion()
			return save()
		}
		if op.Automatic {
			observation := r.Collector.Collect(ctx, f)
			if j.Capacity.LowSince.IsZero() || j.Capacity.LowSamples < f.Spec.Capacity.MinSamples || observation.At.Sub(j.Capacity.LowSince) < capacity.Seconds(f.Spec.Capacity.ScaleInStabilizationSeconds) || !capacity.LowDemand(*f.Spec.Capacity, observation, op.From) || observation.At.After(r.capacityNow()) || r.capacityNow().Sub(observation.At) > capacity.Seconds(f.Spec.Capacity.MaxAgeSeconds) {
				return report("CapacityUncertain", "Fresh sustained low demand required")
			}
		}
		if assessedAt.After(r.capacityNow()) || r.capacityNow().Sub(assessedAt) > 5*time.Second {
			return fail(errors.New("bucket preflight expired before replica issue"))
		}
		if err := r.applyReplicas(ctx, w, op); err != nil {
			return fail(err)
		}
		op.Phase = "Recovering"
		return save()
	}
	if len(op.BucketCandidates) == 0 {
		return fail(errors.New("issued Bucket operation lacks durable candidate admission"))
	}
	if op.Phase != "Recovering" {
		op.Phase = "Recovering"
		return save()
	}
	sessions, at, err := r.bucketAssessment(ctx, f, j, op.To, true)
	if err != nil {
		return fail(err)
	}
	// The same exact observed survivor/retired set must pass both settling checks.
	if !equality.Semantic.DeepEqual(sessions, op.BucketCandidates) {
		op.BucketCandidates = sessions
		op.SettledAt = time.Time{}
		return save()
	}
	if op.SettledAt.IsZero() {
		op.SettledAt = at
		return save()
	}
	if at.Sub(op.SettledAt) < 10*time.Second {
		return report("LifecycleProgress", "Bucket membership converged; repeating fresh survivor and lease checks through settling")
	}
	j.BucketHistory = sessions
	done := completion(op, at)
	done.Outcome = "BucketMembershipConvergedProcessLivenessUnknown"
	j.History = append(j.History, done)
	j.Applied, j.Operation = op.To, nil
	if j.Capacity != nil {
		capacity.RecordAction(j.Capacity, r.capacityNow(), op.From, op.To)
	}
	return save()
}
