package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const applicationMaxAge = 90 * time.Second
const applicationPollInterval = 15 * time.Second

type applicationSample struct {
	started time.Time
	node    fleet.ApplicationNodeStatus
	state   controlplane.Application
	fresh   bool
}

func (r *Reconciler) applicationSample(ctx context.Context, f *fleet.CelldFleet, pod *corev1.Pod) applicationSample {
	s := applicationSample{node: fleet.ApplicationNodeStatus{Name: pod.Name, UID: string(pod.UID), Reason: "Unavailable"}}
	_, started := podIdentity(pod)
	s.started = started
	target, err := runtimeTarget(f, pod, "")
	if err != nil || !podReady(pod) {
		return s
	}
	a, err := r.ApplicationRuntime.Application(ctx, target)
	if err != nil {
		if errors.Is(err, controlplane.ErrUnsupported) {
			s.node.Reason = "Unsupported"
		}
		return s
	}
	s.node.RuntimeGeneration = a.RuntimeGeneration
	if a.Loaded != nil {
		s.node.Version = a.Loaded.Version
	}
	s.state = a
	if !a.Fresh(r.capacityNow(), started, applicationMaxAge) {
		s.node.Reason = "StaleOrIncomplete"
		return s
	}
	s.fresh = true
	switch {
	case a.AdoptionStatus == "failed":
		s.node.Reason = "AdoptionFailed"
	case a.Loaded == nil || *a.Loaded != a.Target.ApplicationVersion || a.AdoptionStatus == "adopting":
		s.node.Reason = "Adopting"
	case a.AdoptionStatus != "adopted" && a.AdoptionStatus != "unchanged":
		s.node.Reason = "StaleOrIncomplete"
		s.fresh = false
	case a.PendingCells > 0 || a.SwappingCells > 0:
		s.node.Reason = "CellsTransitioning"
	default:
		s.node.Reason = "Converged"
	}
	return s
}

// Collect against both an opening and closing membership inventory. A Pod that
// joins, restarts, starts terminating or disappears during the poll invalidates
// convergence, even when all successful responses agree.
func (r *Reconciler) observeApplication(ctx context.Context, f *fleet.CelldFleet) (*fleet.ApplicationStatus, metav1.ConditionStatus, string) {
	out := &fleet.ApplicationStatus{ObservedAt: metav1.NewTime(r.capacityNow())}
	unknown := func(reason string) (*fleet.ApplicationStatus, metav1.ConditionStatus, string) {
		out.Target = nil
		return out, metav1.ConditionUnknown, reason
	}
	if err := f.ValidateRuntime(); err != nil {
		return unknown("InvalidConfiguration")
	}
	if !f.DeletionTimestamp.IsZero() {
		return unknown("FleetDeleting")
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err != nil || w.GetLabels()[FleetLabel] != string(f.UID) || !w.GetDeletionTimestamp().IsZero() {
		return unknown("WorkloadUnavailable")
	}
	out.ExpectedNodes = max(replicas(w), f.Status.DesiredReplicas)
	s := &fleetState{WorkloadUID: w.GetUID()}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return unknown("MembershipUnavailable")
	}
	slices.SortFunc(pods, func(a, b corev1.Pod) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	samples := make([]applicationSample, len(pods))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(8, len(pods)) {
		wg.Go(func() {
			for i := range jobs {
				samples[i] = r.applicationSample(ctx, f, &pods[i])
			}
		})
	}
	for i := range pods {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	out.ObservedAt = metav1.NewTime(r.capacityNow())
	versions := map[controlplane.ApplicationVersion]int32{}
	var target *controlplane.ApplicationVersion
	targetsDiffer, progressing, failed := false, false, false
	for _, sample := range samples {
		if sample.fresh && !sample.state.Fresh(r.capacityNow(), sample.started, applicationMaxAge) {
			sample.fresh = false
			sample.node.Reason = "StaleOrIncomplete"
		}
		out.Nodes = append(out.Nodes, sample.node)
		if !sample.fresh {
			out.UnavailableNodes++
			continue
		}
		out.ObservedNodes++
		a := sample.state
		if a.Loaded != nil {
			versions[*a.Loaded]++
		}
		if target == nil {
			t := a.Target.ApplicationVersion
			target = &t
		} else if *target != a.Target.ApplicationVersion {
			targetsDiffer = true
		}
		out.PendingCells += a.PendingCells
		out.SwappingCells += a.SwappingCells
		progressing = progressing || sample.node.Reason != "Converged"
		failed = failed || sample.node.Reason == "AdoptionFailed"
	}
	for v, count := range versions {
		out.Versions = append(out.Versions, fleet.ApplicationVersionCount{Version: v.Version, Prefix: v.Prefix, Nodes: count})
	}
	slices.SortFunc(out.Versions, func(a, b fleet.ApplicationVersionCount) int {
		if a.Version < b.Version || (a.Version == b.Version && a.Prefix < b.Prefix) {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	})
	out.UnavailableNodes = max(out.UnavailableNodes, out.ExpectedNodes-out.ObservedNodes)
	closing, err := r.currentPods(ctx, f, s)
	if err != nil || !sameApplicationPods(pods, closing) {
		return unknown("MembershipChanged")
	}
	currentWorkload := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), currentWorkload); err != nil || currentWorkload.GetUID() != w.GetUID() || currentWorkload.GetLabels()[FleetLabel] != string(f.UID) || currentWorkload.GetGeneration() != w.GetGeneration() || replicas(currentWorkload) != replicas(w) || !currentWorkload.GetDeletionTimestamp().IsZero() {
		return unknown("MembershipChanged")
	}
	if len(pods) == 0 || len(pods) != int(out.ExpectedNodes) || out.UnavailableNodes != 0 {
		return unknown("IncompleteCoverage")
	}
	if targetsDiffer || target == nil {
		return unknown("TargetsDisagree")
	}
	out.Target = &fleet.ApplicationVersion{Version: target.Version, Prefix: target.Prefix}
	if failed {
		return out, metav1.ConditionFalse, "AdoptionFailed"
	}
	if progressing {
		return out, metav1.ConditionFalse, "Converging"
	}
	return out, metav1.ConditionTrue, "Converged"
}

func sameApplicationPods(before, after []corev1.Pod) bool {
	if len(before) != len(after) {
		return false
	}
	byName := map[string]corev1.Pod{}
	for _, p := range before {
		byName[p.Name] = p
	}
	for _, p := range after {
		old, ok := byName[p.Name]
		oldID, _ := podIdentity(&old)
		id, _ := podIdentity(&p)
		if !ok || old.UID != p.UID || oldID != id || old.Status.PodIP != p.Status.PodIP || podReady(&old) != podReady(&p) || !equality.Semantic.DeepEqual(old.DeletionTimestamp, p.DeletionTimestamp) || !equality.Semantic.DeepEqual(old.OwnerReferences, p.OwnerReferences) {
			return false
		}
	}
	return true
}

func (r *Reconciler) reportApplication(ctx context.Context, key client.ObjectKey) error {
	f := &fleet.CelldFleet{}
	if err := r.Get(ctx, key, f); err != nil {
		return client.IgnoreNotFound(err)
	}
	f.Default()
	if previous := f.Status.Application; previous != nil && f.DeletionTimestamp.IsZero() && f.Status.ObservedGeneration == f.Generation && r.capacityNow().Sub(previous.ObservedAt.Time) >= 0 && r.capacityNow().Sub(previous.ObservedAt.Time) < applicationPollInterval {
		return nil
	}
	before := f.DeepCopy()
	collectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	application, status, reason := r.observeApplication(collectCtx, f)
	cancel()
	f.Status.Application = application
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: "ApplicationConverged", Status: status, Reason: reason, Message: "Application convergence is relative to fresh observed deployment targets; inspect status.application for coverage and node details", ObservedGeneration: f.Generation})
	return r.Status().Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
