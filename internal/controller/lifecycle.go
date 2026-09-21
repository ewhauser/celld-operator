package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) lifecycle(ctx context.Context, f *fleet.CelldFleet, h *loadedState, w client.Object) (ctrl.Result, bool, error) {
	block := func(reason string, err error) (ctrl.Result, bool, error) {
		v, e := r.report(ctx, f, h, reason, err.Error(), false)
		return v, true, e
	}
	save := func() (ctrl.Result, bool, error) {
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveState(ctx, h.res, h.j)
	}
	if h.err != nil {
		return block("OperationInvalid", h.err)
	}
	if h.res.Annotations[attemptAnnotation] == "" {
		return block("LifecycleBlocked", errors.New("missing workload creation authority"))
	}
	if h.j == nil {
		baseline := f.DeepCopy()
		baseline.Spec.Replicas = replicas(w)
		if !matches(workload(baseline, r.Options), w) || w.GetAnnotations()[operationKey] != "" || w.GetUID() == "" {
			return block("LifecycleBlocked", errors.New("workload drift or lost operation authority"))
		}
		s := &fleetState{Version: 1, FleetUID: f.UID, WorkloadUID: w.GetUID(), Initial: h.res.Spec.InitialReplicas, Applied: replicas(w), RuntimeImage: runtimeImage(f), Claims: map[string]types.UID{}}
		if f.Spec.Profile == "PersistentFleet" {
			if err := json.Unmarshal([]byte(h.res.Annotations[creationClaimsKey]), &s.Claims); err != nil {
				return block("StorageIdentityConflict", err)
			}
			if len(s.Claims) != int(s.Applied) {
				return block("StorageIdentityConflict", errors.New("initial claim inventory incomplete"))
			}
		}
		if err := r.verifyClaims(ctx, f, s); err != nil {
			return block("StorageIdentityConflict", err)
		}
		h.j = s
		delete(h.res.Annotations, creationClaimsKey)
		if err := r.saveState(ctx, h.res, s); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	s := h.j
	if s.FleetUID != f.UID || s.WorkloadUID != w.GetUID() || !w.GetDeletionTimestamp().IsZero() {
		return block("LifecycleBlocked", errors.New("fleet or workload identity changed"))
	}
	if err := r.verifyWorkload(f, s, w); err != nil {
		return block("LifecycleBlocked", err)
	}
	if s.Operation == nil {
		if err := r.verifyClaims(ctx, f, s); err != nil {
			return block("StorageIdentityConflict", err)
		}
		if err := r.scheduleCurrent(ctx, f, s); err != nil {
			return block("SchedulingBlocked", err)
		}
		if paused(f) {
			resetMaintenanceCapacity(s)
			return block("MaintenancePaused", errors.New("new operations suspended"))
		}
		kind, target, token := requestedMaintenance(f, s)
		if kind == "" {
			before := stateRendering(s)
			var automatic bool
			target, automatic = r.capacityTarget(ctx, f, s)
			if target == s.Applied {
				if !sameState(before, s) {
					if err := r.saveState(ctx, h.res, s); err != nil {
						return ctrl.Result{}, true, err
					}
				}
				return ctrl.Result{}, false, nil
			}
			if err := r.sameFleetRequest(ctx, f); err != nil {
				return block("CapacityChanged", err)
			}
			o := newOperation(f, s, w, r.capacityNow(), "Scale", target, "")
			o.Automatic = automatic
			if automatic {
				o.PolicyHash = policyHash(*f.Spec.Capacity)
			}
			if target < s.Applied {
				o.To = s.Applied - 1
			}
			s.Operation = o
		} else {
			if kind != "Delete" && (f.Spec.Maintenance == nil || !f.Spec.Maintenance.AllowCoordinatedDowntime) {
				return block("DisruptionBlocked", errors.New("restart and upgrade require coordinated downtime permission"))
			}
			if kind == "Upgrade" && !knownRuntime(runtimeImage(f)) {
				return block("UnsupportedTransition", errors.New("target requires an independently qualified fork digest"))
			}
			s.Operation = newOperation(f, s, w, r.capacityNow(), kind, target, token)
			resetMaintenanceCapacity(s)
		}
		o := s.Operation
		if o.Kind == "Scale" && o.To > o.From {
			if err := r.freshGrowth(ctx, f, o); err != nil {
				s.Operation = nil
				return block("StorageIdentityConflict", err)
			}
		} else {
			if r.Options.LauncherImage == "" {
				s.Operation = nil
				return block("StrictShutdownUnavailable", errors.New("launcher required for strict disk removal"))
			}
			if o.Kind == "Scale" {
				if _, ok := w.(*appsv1.StatefulSet); !ok {
					s.Operation = nil
					return block("OrderedWorkloadRequired", errors.New("deployment cannot bind replica contraction to one exact pod; use Ordered Bucket for contraction"))
				}
			}
			targets, err := r.captureTargets(ctx, f, s, w)
			if err != nil {
				s.Operation = nil
				return block("RemovalBlocked", err)
			}
			o.Targets = targets
			if o.Kind == "Scale" {
				if err := r.survivorCapacity(ctx, f, s, o); err != nil {
					s.Operation = nil
					return block("CapacityUncertain", err)
				}
			}
		}
		r.faultPoint("before-intent")
		if err := r.saveState(ctx, h.res, s); err != nil {
			return ctrl.Result{}, true, err
		}
		r.faultPoint("after-intent")
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	o := s.Operation
	switch o.Phase {
	case "Intent":
		// This CAS is the only cancellation boundary. A Requesting writer and a
		// cancellation writer cannot both win it. No external action precedes it.
		if paused(f) || requestChanged(f, s, o) || !r.capacityNow().Before(o.Deadline) {
			s.Completion = &operationCompletion{ID: o.ID, Kind: o.Kind, Outcome: "CanceledBeforeIssue", At: r.capacityNow()}
			s.Operation = nil
			resetMaintenanceCapacity(s)
			return save()
		}
		if err := r.sameFleetRequest(ctx, f); err != nil {
			return block("RequestChanged", err)
		}
		if o.Kind == "Scale" && o.To < o.From {
			if err := r.survivorCapacity(ctx, f, s, o); err != nil {
				return block("CapacityUncertain", err)
			}
		}
		for _, t := range o.Targets {
			if _, err := r.currentTarget(ctx, f, s, t); err != nil {
				return block("IdentityChanged", err)
			}
		}
		if o.To > o.From {
			if err := r.freshGrowth(ctx, f, o); err != nil {
				return block("StorageIdentityConflict", err)
			}
		}
		o.Phase = "Requesting"
		return save()
	case "Requesting":
		for i := range o.Targets {
			t := &o.Targets[i]
			if t.Proof != nil {
				continue
			}
			pod, err := r.currentTarget(ctx, f, s, *t)
			if err != nil {
				return block("IdentityChanged", err)
			}
			// Observe after expiry, never extend or reissue the fixed deadline.
			operation := ""
			callCtx := ctx
			cancel := func() {}
			if r.capacityNow().Before(o.Deadline) {
				operation = o.ID
				callCtx, cancel = context.WithDeadline(ctx, o.Deadline)
				callCtx = withRemovalDeadline(callCtx, o.Deadline)
			}
			r.faultPoint("before-request")
			result, err := r.callLauncher(callCtx, f, pod, operation, t.Identity.Generation)
			cancel()
			r.faultPoint("after-request")
			if err != nil {
				return block("ShutdownUncertain", err)
			}
			if !sameProcess(*t, result) {
				o.Phase = "Blocked"
				o.Blocker = "launcher invocation changed before proof capture"
				return save()
			}
			if result.Phase == "Failed" || result.Phase == "ExitedUnrequested" || result.Phase == "Blocked" || result.Removal.Phase == "failed" {
				o.Phase = "Blocked"
				o.Blocker = "terminal launcher failure: " + result.Error
				return save()
			}
			if !validLauncherProof(o, *t, result) {
				return block("ShutdownPending", errors.New("strict runtime completion, exact child exit, inherited-lock release and restart denial are all required"))
			}
			if _, err := r.currentTarget(ctx, f, s, *t); err != nil {
				return block("IdentityChanged", err)
			}
			t.Proof = proofFrom(result)
			r.faultPoint("before-proof")
			if err := r.saveState(ctx, h.res, s); err != nil {
				return ctrl.Result{}, true, err
			}
			r.faultPoint("after-proof")
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		o.Phase = "ProofCaptured"
		return save()
	case "ProofCaptured", "Apply":
		return r.applyOperation(ctx, f, h, w, false)
	case "Observing":
		if err := r.effectObserved(ctx, f, s, w, false); err != nil {
			return block("EffectPending", err)
		}
		if o.Kind == "Scale" && o.To > o.From {
			o.Phase = "Joining"
			return save()
		}
		o.Phase = "DeleteClaims"
		return save()
	case "DeleteClaims":
		done, err := r.cleanupClaims(ctx, f, h)
		if err != nil {
			return block("StorageCleanupBlocked", err)
		}
		if !done {
			v, e := r.report(ctx, f, h, "LifecycleProgress", "Waiting for exact PVC/PV deletion and CSI attachment removal", true)
			return v, true, e
		}
		if o.Kind == "Delete" || o.Kind == "Scale" {
			return r.completeOperation(ctx, f, h, w)
		}
		o.Phase = "Resume"
		o.PreviousEffect = o.ID + "/stop"
		o.EffectVersion = ""
		return save()
	case "Resume", "ApplyResume":
		return r.applyOperation(ctx, f, h, w, true)
	case "Joining":
		if err := r.admitNewClaims(ctx, f, h); err != nil {
			return block("StorageIdentityConflict", err)
		}
		if err := r.scheduleCurrent(ctx, f, s); err != nil {
			return block("SchedulingBlocked", err)
		}
		if err := r.effectObserved(ctx, f, s, w, o.Kind != "Scale"); err != nil {
			return block("EffectPending", err)
		}
		if readyReplicas(w) != o.To {
			return block("Joining", errors.New("waiting for specified replica readiness"))
		}
		return r.completeOperation(ctx, f, h, w)
	case "Blocked":
		return block("OperationBlocked", errors.New(o.Blocker))
	default:
		return block("OperationInvalid", errors.New("unknown operation phase"))
	}
}
func newOperation(f *fleet.CelldFleet, s *fleetState, w client.Object, now time.Time, kind string, target int32, token string) *currentOperation {
	return &currentOperation{ID: string(uuid.NewUUID()), Kind: kind, Phase: "Intent", StartedAt: now, Deadline: now.Add(operationBudget), FleetUID: f.UID, WorkloadUID: w.GetUID(), From: s.Applied, To: target, SourceImage: s.RuntimeImage, TargetImage: runtimeImage(f), RestartToken: token, ManualBaseline: f.Spec.Replicas, PreviousEffect: w.GetAnnotations()[operationKey]}
}
func requestChanged(f *fleet.CelldFleet, s *fleetState, o *currentOperation) bool {
	kind, target, token := requestedMaintenance(f, s)
	if o.Kind != "Scale" {
		return kind != o.Kind || target != o.To || token != o.RestartToken || runtimeImage(f) != o.TargetImage
	}
	if kind != "" || o.ManualBaseline != f.Spec.Replicas {
		return true
	}
	return o.Automatic && (f.Spec.Capacity == nil || policyHash(*f.Spec.Capacity) != o.PolicyHash)
}
func (r *Reconciler) sameFleetRequest(ctx context.Context, f *fleet.CelldFleet) error {
	live := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), live); err != nil {
		return err
	}
	live.Default()
	if live.UID != f.UID || !equality.Semantic.DeepEqual(live.Spec, f.Spec) || !live.DeletionTimestamp.Equal(f.DeletionTimestamp) {
		return errors.New("fleet request changed during observation")
	}
	return nil
}
func (r *Reconciler) verifyWorkload(f *fleet.CelldFleet, s *fleetState, w client.Object) error {
	n, image := s.Applied, s.RuntimeImage
	if o := s.Operation; o != nil {
		marker := w.GetAnnotations()[operationKey]
		if marker == effectID(o, false) {
			n = effectCount(o, false)
		}
		if marker == effectID(o, true) && o.Kind != "Scale" {
			n = o.To
			image = o.TargetImage
		}
	}
	expected := f.DeepCopy()
	expected.Spec.Replicas = n
	expected.Spec.RuntimeImage = image
	if !matches(workload(expected, r.Options), w) {
		return errors.New("workload differs from exact recorded infrastructure")
	}
	return nil
}
func effectID(o *currentOperation, resume bool) string {
	if resume {
		return o.ID + "/resume"
	}
	return o.ID + "/stop"
}
func effectCount(o *currentOperation, resume bool) int32 {
	if resume || o.Kind == "Scale" {
		return o.To
	}
	return 0
}
func (r *Reconciler) applyOperation(ctx context.Context, f *fleet.CelldFleet, h *loadedState, w client.Object, resume bool) (ctrl.Result, bool, error) {
	s, o := h.j, h.j.Operation
	block := func(err error) (ctrl.Result, bool, error) {
		v, e := r.report(ctx, f, h, "EffectBlocked", err.Error(), false)
		return v, true, e
	}
	n, id := effectCount(o, resume), effectID(o, resume)
	if w.GetUID() != o.WorkloadUID {
		return block(errors.New("workload UID changed"))
	}
	if w.GetAnnotations()[operationKey] == id && replicas(w) == n {
		if resume {
			o.Phase = "Joining"
		} else {
			o.Phase = "Observing"
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveState(ctx, h.res, s)
	}
	expected := o.From
	if resume {
		expected = 0
	}
	if replicas(w) != expected || w.GetAnnotations()[operationKey] != o.PreviousEffect {
		return block(errors.New("effect predecessor changed; refusing old intent"))
	}
	// Every re-arm validates the exact current operation, predecessor, template,
	// count and target disks before recording a new CAS version. Never retry an
	// Update directly with a fresh workload resourceVersion.
	if err := r.verifyWorkload(f, s, w); err != nil {
		return block(err)
	}
	if !resume {
		if len(o.Targets) > 0 {
			if err := r.verifyRemovalWorkingSet(ctx, f, s, o); err != nil {
				return block(err)
			}
		}
		for _, t := range o.Targets {
			if t.Proof == nil || !validProof(o, t, *t.Proof) {
				return block(errors.New("missing bound runtime/process proof"))
			}
			if _, err := r.currentTarget(ctx, f, s, t); err != nil {
				return block(err)
			}
		}
	}
	if (resume || o.To > o.From) && f.Spec.Profile == "PersistentFleet" {
		if err := r.freshGrowth(ctx, f, o); err != nil {
			return block(err)
		}
	}
	phase := "Apply"
	if resume {
		phase = "ApplyResume"
	}
	if o.Phase != phase || o.EffectVersion != w.GetResourceVersion() {
		o.Phase = phase
		o.EffectVersion = w.GetResourceVersion()
		if err := r.saveState(ctx, h.res, s); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	// Recheck reservation to reject a stale caller before touching infrastructure.
	// The workload CAS independently fences delayed requests at the effect itself.
	live := &fleet.CelldStorageReservation{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(h.res), live); err != nil {
		return ctrl.Result{}, true, err
	}
	if live.ResourceVersion != h.res.ResourceVersion {
		return block(errors.New("operation authority changed before effect"))
	}
	setReplicas(w, n)
	if w.GetAnnotations() == nil {
		w.SetAnnotations(map[string]string{})
	}
	w.GetAnnotations()[operationKey] = id
	if resume {
		switch w := w.(type) {
		case *appsv1.StatefulSet:
			w.Spec.Template.Spec.Containers[0].Image = o.TargetImage
		case *appsv1.Deployment:
			w.Spec.Template.Spec.Containers[0].Image = o.TargetImage
		}
	}
	r.faultPoint("before-effect")
	if err := r.Update(ctx, w); err != nil {
		return ctrl.Result{}, true, err
	}
	r.faultPoint("after-effect")
	return ctrl.Result{RequeueAfter: time.Second}, true, nil
}
func (r *Reconciler) effectObserved(ctx context.Context, f *fleet.CelldFleet, s *fleetState, w client.Object, resume bool) error {
	o := s.Operation
	if w.GetUID() != o.WorkloadUID || w.GetAnnotations()[operationKey] != effectID(o, resume) || replicas(w) != effectCount(o, resume) {
		return errors.New("exact workload effect not observed")
	}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return err
	}
	if len(pods) != int(effectCount(o, resume)) {
		return errors.New("pod count has not converged")
	}
	if !resume && (o.Kind != "Scale" || o.To <= o.From) {
		for _, p := range pods {
			for _, t := range o.Targets {
				if p.Name == t.Pod || p.UID == t.PodUID {
					return errors.New("removed pod still exists or has a replacement")
				}
			}
		}
	}
	return nil
}
func (r *Reconciler) completeOperation(ctx context.Context, f *fleet.CelldFleet, h *loadedState, w client.Object) (ctrl.Result, bool, error) {
	s, o := h.j, h.j.Operation
	if err := r.effectObserved(ctx, f, s, w, o.Kind != "Scale" && o.Kind != "Delete"); err != nil {
		v, e := r.report(ctx, f, h, "EffectPending", err.Error(), false)
		return v, true, e
	}
	s.Completion = &operationCompletion{ID: o.ID, Kind: o.Kind, Outcome: "Completed", At: r.capacityNow()}
	s.Applied = o.To
	if o.Kind == "Restart" {
		s.RestartToken = o.RestartToken
	}
	if o.Kind == "Upgrade" {
		s.RuntimeImage = o.TargetImage
		s.RestartToken = o.RestartToken
	}
	if s.Capacity != nil {
		capacity.RecordAction(s.Capacity, r.capacityNow(), o.From, o.To)
	}
	s.Operation = nil
	resetMaintenanceCapacity(s)
	return ctrl.Result{RequeueAfter: time.Second}, true, r.saveState(ctx, h.res, s)
}
func (r *Reconciler) currentPods(ctx context.Context, f *fleet.CelldFleet, s *fleetState) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f)), client.Limit(101)); err != nil {
		return nil, err
	}
	if list.Continue != "" || len(list.Items) > 100 {
		return nil, errors.New("pod working set exceeds fleet limit")
	}
	for _, p := range list.Items {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.APIVersion != "apps/v1" {
			return nil, errors.New("pod owner missing")
		}
		switch {
		case owner.Kind == "StatefulSet":
			if owner.UID != s.WorkloadUID || owner.Name != f.Name {
				return nil, errors.New("pod workload identity changed")
			}
		case owner.Kind == "ReplicaSet" && f.Spec.Profile == "Bucket" && !orderedBucket(f):
			rs := &appsv1.ReplicaSet{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: owner.Name}, rs); err != nil {
				return nil, err
			}
			parent := metav1.GetControllerOf(rs)
			if rs.UID != owner.UID || parent == nil || parent.UID != s.WorkloadUID || parent.Name != f.Name || parent.Kind != "Deployment" || !rs.DeletionTimestamp.IsZero() {
				return nil, errors.New("ReplicaSet ownership changed")
			}
		default:
			return nil, errors.New("unexpected pod controller")
		}
	}
	return list.Items, nil
}
func (r *Reconciler) captureTargets(ctx context.Context, f *fleet.CelldFleet, s *fleetState, _ client.Object) ([]operationTarget, error) {
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return nil, err
	}
	if len(pods) != int(s.Applied) {
		return nil, errors.New("current membership has not converged")
	}
	o := s.Operation
	targets := []operationTarget{}
	for i := range pods {
		p := &pods[i]
		if o.Kind == "Scale" && p.Name != fmt.Sprintf("%s-%d", f.Name, o.From-1) {
			continue
		}
		if err := validatePersistentPod(appliedRuntime(f, s), p, r.Options); err != nil {
			return nil, err
		}
		container, _ := podIdentity(p)
		if container == "" || !podReady(p) {
			return nil, errors.New("target is not a current ready invocation")
		}
		node := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, node); err != nil {
			return nil, err
		}
		if !healthyHost(node) {
			return nil, errors.New("host incarnation unavailable")
		}
		id, err := r.callLauncher(ctx, f, p, "", "")
		if err != nil {
			return nil, err
		}
		if id.Phase != "Running" || id.Operation != "" || id.PodUID != string(p.UID) || id.Node != runtimeNode(f, p) || id.Host != p.Spec.NodeName || id.BootID != node.Status.NodeInfo.BootID || id.Generation == "" || id.Invocation == "" || id.DiskID == "" || id.PID <= 0 {
			return nil, errors.New("launcher discovery is not an exact running child")
		}
		t := operationTarget{Pod: p.Name, PodUID: p.UID, IP: p.Status.PodIP, Container: container, HostUID: string(node.UID), Identity: processFrom(id)}
		if f.Spec.Profile == "PersistentFleet" {
			v, err := r.targetVolume(ctx, f, s, p.Name)
			if err != nil {
				return nil, err
			}
			t.Storage = v
		}
		if _, err := r.currentTarget(ctx, f, s, t); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	expected := int(o.From)
	if o.Kind == "Scale" {
		expected = 1
	}
	if len(targets) != expected {
		return nil, errors.New("exact removal target is missing from current membership")
	}
	slices.SortFunc(targets, func(a, b operationTarget) int {
		if a.Pod < b.Pod {
			return -1
		}
		if a.Pod > b.Pod {
			return 1
		}
		return 0
	})
	return targets, nil
}
func (r *Reconciler) currentTarget(ctx context.Context, f *fleet.CelldFleet, s *fleetState, t operationTarget) (*corev1.Pod, error) {
	p := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: t.Pod}, p); err != nil {
		return nil, err
	}
	container, _ := podIdentity(p)
	if p.UID != t.PodUID || p.Spec.NodeName != t.Identity.Host || (t.Proof == nil && (p.Status.PodIP != t.IP || container != t.Container)) || p.Labels[FleetLabel] != string(f.UID) {
		return nil, errors.New("target pod or launcher container changed")
	}
	if err := validatePersistentPod(appliedRuntime(f, s), p, r.Options); err != nil {
		return nil, err
	}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.UID == t.PodUID }) {
		return nil, errors.New("target no longer belongs to workload")
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: t.Identity.Host}, node); err != nil {
		return nil, err
	}
	if string(node.UID) != t.HostUID || node.Status.NodeInfo.BootID != t.Identity.BootID {
		return nil, errors.New("host incarnation changed")
	}
	if t.Storage != nil {
		v, err := r.targetVolume(ctx, f, s, t.Pod)
		if err != nil {
			return nil, err
		}
		if !sameVolume(*t.Storage, *v) {
			return nil, errors.New("target disk identity changed")
		}
	}
	return p, nil
}
func (r *Reconciler) survivorCapacity(ctx context.Context, f *fleet.CelldFleet, s *fleetState, o *currentOperation) error {
	if r.Collector == nil {
		return errors.New("survivor metrics unavailable")
	}
	policy := appliedRuntime(f, s)
	if policy.Spec.Capacity == nil {
		policy.Spec.Capacity = &fleet.CapacityPolicy{}
		policy.Spec.Capacity.Default()
	}
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return err
	}
	if len(pods) != int(o.From) {
		return errors.New("membership changed before shutdown")
	}
	ids := []string{}
	zones := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		id, _ := podIdentity(p)
		ids = append(ids, id)
		if p.UID != o.Targets[0].PodUID {
			n := &corev1.Node{}
			if err := r.Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, n); err != nil {
				return err
			}
			zones[n.Labels[corev1.LabelTopologyZone]] = true
		}
	}
	if f.Spec.Placement.Mode != "Relaxed" {
		for _, z := range f.Spec.Placement.Zones {
			if !zones[z] {
				return errors.New("removal would lose requested zone coverage")
			}
		}
	}
	return ValidateSurvivors(*policy.Spec.Capacity, r.Collector.Collect(ctx, policy), ids, o.Targets[0].Container, r.capacityNow())
}

func policyHash(p fleet.CapacityPolicy) string { b, _ := json.Marshal(p); return digest(b) }

// A replica effect must not sweep up an unexpected Pod that was never stopped.
// StatefulSets can remove every out-of-range ordinal, and a Deployment stop
// affects every replica, so count alone is insufficient at this boundary.
func (r *Reconciler) verifyRemovalWorkingSet(ctx context.Context, f *fleet.CelldFleet, s *fleetState, o *currentOperation) error {
	pods, err := r.currentPods(ctx, f, s)
	if err != nil {
		return err
	}
	if len(pods) != int(o.From) {
		return errors.New("removal working set changed before effect")
	}
	for _, p := range pods {
		if !p.DeletionTimestamp.IsZero() {
			return errors.New("removal working set is terminating")
		}
		if f.Spec.Profile == "PersistentFleet" || orderedBucket(f) {
			n, err := bucketOrdinal(f, p.Name)
			if err != nil || n >= int(o.From) {
				return errors.New("unexpected ordinal would be removed without proof")
			}
		}
		if o.Kind != "Scale" && !slices.ContainsFunc(o.Targets, func(t operationTarget) bool { return t.PodUID == p.UID && t.Pod == p.Name }) {
			return errors.New("maintenance acquired an uncaptured pod")
		}
	}
	return nil
}
