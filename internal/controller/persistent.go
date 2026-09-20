package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/catalog"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type persistentMember struct {
	ProviderID                                                                                                                        string
	Node, PodUID, Container, Host, HostUID, Hostname, BootID, Zone, Invocation, Generation, ClaimUID, VolumeUID, VolumeHandle, DiskID string
	Epoch                                                                                                                             uint64
	Ensemble                                                                                                                          []string
	Stopped, Retired, RestartDenied                                                                                                   bool
	// Superseded marks a survivor invocation whose pod was recreated and admitted
	// back onto the same host incarnation and retained volume. Exclusion authority
	// is the successor launcher's exclusive lock, not a stop receipt.
	Superseded bool
}

// resolvedMember reports whether a historical invocation no longer needs a live
// record: positively retired through the launcher, or superseded on the same host.
func resolvedMember(m persistentMember) bool {
	return (m.Retired && m.Stopped && m.RestartDenied) || m.Superseded
}

// stopExpiryGrace bounds clock skew between the manager and a launcher. A stop
// request carries NotAfterMS no later than the operation deadline, so once the
// launcher still reports Running this long after the deadline, no request for
// this operation can be accepted anymore and the removal is provably unissued.
const stopExpiryGrace = time.Minute

func sameInvocation(a, b persistentMember) bool {
	return a.Node == b.Node && a.PodUID == b.PodUID && a.Container == b.Container && a.Host == b.Host && a.HostUID == b.HostUID && a.BootID == b.BootID && a.Invocation == b.Invocation && a.Generation == b.Generation && a.ClaimUID == b.ClaimUID && a.VolumeUID == b.VolumeUID && a.VolumeHandle == b.VolumeHandle && a.DiskID == b.DiskID
}
func validateReactivation(j *lifecycleJournal, from, to int32, name string) error {
	for ordinal := from; ordinal < to; ordinal++ {
		node := fmt.Sprintf("%s-%d", name, ordinal)
		for _, s := range j.Inventory.Sessions {
			if s.Node != node {
				continue
			}
			if !slices.ContainsFunc(j.PersistentHistory, func(p persistentMember) bool {
				return p.Node == node && p.Generation == s.Generation && p.Retired && p.Stopped && p.RestartDenied
			}) {
				return errors.New("retained identity lacks positively completed launcher retirement")
			}
		}
	}
	return nil
}
func validatePersistentPod(f *fleet.CelldFleet, pod *corev1.Pod, opts Options) error {
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.InitContainers) != 1 || pod.Spec.HostPID || pod.Spec.HostNetwork || pod.Spec.HostIPC || !pod.DeletionTimestamp.IsZero() {
		return errors.New("unqualified persistent pod composition")
	}
	expected := podTemplate(f, opts).Spec
	actual := *pod.Spec.DeepCopy()
	normalizePod(&expected)
	normalizePod(&actual)
	got, want := actual.Containers[0], expected.Containers[0]
	if !equality.Semantic.DeepEqual(got.SecurityContext, want.SecurityContext) || !equality.Semantic.DeepEqual(actual.SecurityContext, expected.SecurityContext) || !equality.Semantic.DeepEqual(actual.AutomountServiceAccountToken, expected.AutomountServiceAccountToken) || got.Image != runtimeImage(f) || !slices.Equal(got.Command, want.Command) || len(got.Args) != 0 || len(got.EnvFrom) != 0 || got.WorkingDir != "" || actual.ServiceAccountName != expected.ServiceAccountName {
		return errors.New("persistent runtime invocation differs")
	}
	init := actual.InitContainers[0]
	wantInit := expected.InitContainers[0]
	if !equality.Semantic.DeepEqual(init.SecurityContext, wantInit.SecurityContext) || init.Name != wantInit.Name || init.Image != wantInit.Image || !slices.Equal(init.Command, wantInit.Command) || len(init.Args) != 0 || len(init.Env) != 0 || len(init.EnvFrom) != 0 || !equality.Semantic.DeepEqual(init.VolumeMounts, wantInit.VolumeMounts) {
		return errors.New("launcher installer differs")
	}
	seen := map[string]bool{}
	for _, env := range got.Env {
		if seen[env.Name] {
			return errors.New("duplicate runtime environment")
		}
		seen[env.Name] = true
		index := slices.IndexFunc(want.Env, func(e corev1.EnvVar) bool { return e.Name == env.Name })
		if index >= 0 {
			if !equality.Semantic.DeepEqual(env, want.Env[index]) {
				return errors.New("runtime environment differs")
			}
		} else {
			switch env.Name {
			case "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_STS_REGIONAL_ENDPOINTS", "AWS_DEFAULT_REGION":
			default:
				return errors.New("unqualified runtime environment")
			}
		}
	}
	for _, env := range want.Env {
		if !seen[env.Name] {
			return errors.New("runtime environment missing")
		}
	}
	for _, mount := range want.VolumeMounts {
		if !slices.ContainsFunc(got.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, mount) }) {
			return errors.New("runtime mount differs")
		}
	}
	for _, mount := range got.VolumeMounts {
		if slices.ContainsFunc(want.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, mount) }) {
			continue
		}
		if !mount.ReadOnly || (mount.MountPath != "/var/run/secrets/eks.amazonaws.com/serviceaccount" && mount.MountPath != "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount") {
			return errors.New("unqualified runtime mount")
		}
	}
	for _, volume := range expected.Volumes {
		if !slices.ContainsFunc(actual.Volumes, func(v corev1.Volume) bool { return equality.Semantic.DeepEqual(v, volume) }) {
			return errors.New("launcher volume differs")
		}
	}
	return nil
}
func (r *Reconciler) persistentMembers(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, count int32, stopping bool) ([]persistentMember, error) {
	f = evidenceRuntime(f, j)
	pods, err := r.Evidence.pods(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(pods) != int(count) {
		return nil, errors.New("PersistentFleet membership has not converged")
	}
	members := make([]persistentMember, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != "StatefulSet" || owner.UID != j.WorkloadUID {
			return nil, errors.New("persistent workload ownership changed")
		}
		if stopping && j.Operation != nil && pod.Name == j.Operation.TargetPod {
			ix := slices.IndexFunc(j.Operation.PersistentMembers, func(m persistentMember) bool { return m.Node == pod.Name && m.PodUID == string(pod.UID) })
			if ix >= 0 && certifiedInfrastructureFence(j, j.Operation.PersistentMembers[ix]) {
				member := j.Operation.PersistentMembers[ix]
				claim := &corev1.PersistentVolumeClaim{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: "data-" + member.Node}, claim); err != nil {
					return nil, err
				}
				uid, handle, err := r.persistentVolumeIdentity(ctx, claim)
				if err != nil {
					return nil, err
				}
				if string(claim.UID) != member.ClaimUID || uid != member.VolumeUID || handle != member.VolumeHandle {
					return nil, errors.New("fenced writer retained disk changed")
				}
				member.Stopped = true
				member.RestartDenied = true // irreversible instance termination, not a launcher receipt
				members = append(members, member)
				continue
			}
		}
		if err := validatePersistentPod(f, pod, r.Options); err != nil {
			return nil, err
		}
		id, _ := podIdentity(pod)
		// A Pod UID alone cannot authenticate an older launcher left behind
		// by an unexpected container restart. This graceful path admits only
		// the first invocation; controlled reuse gets a new Pod UID.
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "celld" && status.RestartCount != 0 {
				return nil, errors.New("unexpected container restart is outside graceful launcher authority")
			}
		}
		if id == "" {
			return nil, errors.New("container identity unavailable")
		}
		node := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
			return nil, err
		}
		if !healthyHost(node) {
			return nil, errors.New("persistent host identity or readiness unavailable")
		}
		claimName := "data-" + pod.Name
		claim := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: claimName}, claim); err != nil {
			return nil, err
		}
		if claim.UID == "" || claim.UID != j.Claims[claimName] || !slices.Equal(claim.Spec.AccessModes, persistentAccessModes(r.Options)) || !claim.DeletionTimestamp.IsZero() {
			return nil, errors.New("retained PVC identity or access mode changed")
		}
		if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
			return v.Name == "data" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName && !v.PersistentVolumeClaim.ReadOnly
		}) {
			return nil, errors.New("persistent data volume association changed")
		}
		volumeUID, volumeHandle, err := r.persistentVolumeIdentity(ctx, claim)
		if err != nil {
			return nil, err
		}
		state, err := r.callLauncher(ctx, f, pod, "", "")
		if err != nil {
			return nil, err
		}
		if state.BootID != "" && state.BootID != node.Status.NodeInfo.BootID {
			return nil, errors.New("authenticated launcher boot differs from Kubernetes host incarnation")
		}
		isDonor := stopping && pod.Name == j.Operation.TargetPod
		if isDonor {
			if state.Phase != "Stopped" || state.Operation != j.Operation.ID || !state.RestartDenied {
				return nil, errors.New("exact child termination and inherited-lock release unconfirmed")
			}
		} else if state.Phase != "Running" || !podReady(pod) {
			return nil, errors.New("persistent survivor launcher is not running and ready")
		}
		members = append(members, persistentMember{ProviderID: node.Spec.ProviderID, Node: pod.Name, PodUID: string(pod.UID), Container: id, Host: node.Name, HostUID: string(node.UID), Hostname: node.Labels[corev1.LabelHostname], BootID: node.Status.NodeInfo.BootID, Zone: node.Labels[corev1.LabelTopologyZone], Invocation: state.Invocation, Generation: state.Generation, ClaimUID: string(claim.UID), VolumeUID: volumeUID, VolumeHandle: volumeHandle, Stopped: isDonor, RestartDenied: isDonor && state.RestartDenied, DiskID: state.DiskID})
	}
	slices.SortFunc(members, func(a, b persistentMember) int {
		if a.Node < b.Node {
			return -1
		}
		if a.Node > b.Node {
			return 1
		}
		return 0
	})
	for ordinal := range count {
		if !slices.ContainsFunc(members, func(m persistentMember) bool { return m.Node == fmt.Sprintf("%s-%d", f.Name, ordinal) }) {
			return nil, errors.New("persistent ordinal membership changed")
		}
	}
	return members, nil
}
func (r *Reconciler) assessPersistent(ctx context.Context, f *fleet.CelldFleet, j *lifecycleJournal, stopping, issued bool) ([]persistentMember, time.Time, error) {
	op := j.Operation
	count := op.From
	if issued {
		count = op.To
	}
	members, err := r.persistentMembers(ctx, f, j, count, stopping)
	if err != nil {
		return nil, time.Time{}, err
	}
	if stopping {
		for _, old := range op.PersistentMembers {
			if issued && old.Node == op.TargetPod {
				if !old.Stopped || !old.RestartDenied {
					return nil, time.Time{}, errors.New("missing durable termination receipt")
				}
				continue
			}
			if !slices.ContainsFunc(members, func(m persistentMember) bool { return sameInvocation(m, old) }) {
				return nil, time.Time{}, errors.New("captured persistent invocation changed")
			}
		}
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return nil, time.Time{}, err
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return nil, time.Time{}, err
	}
	inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
	if err != nil {
		return nil, time.Time{}, err
	}
	history := slices.Clone(j.PersistentHistory)
	for _, m := range op.PersistentMembers {
		if !slices.ContainsFunc(history, func(old persistentMember) bool { return old.Node == m.Node && old.Generation == m.Generation }) {
			history = append(history, m)
		}
	}
	for _, s := range j.Inventory.Sessions {
		if !slices.ContainsFunc(members, func(m persistentMember) bool { return m.Node == s.Node && m.Generation == s.Generation }) && !slices.ContainsFunc(history, func(m persistentMember) bool {
			return m.Node == s.Node && m.Generation == s.Generation && (resolvedMember(m) || (stopping && m.Node == op.TargetPod))
		}) {
			return nil, time.Time{}, errors.New("unresolved historical persistent generation")
		}
	}
	for _, n := range inventory.Nodes {
		for _, prior := range j.Inventory.Sessions {
			if prior.Node == n.Name && prior.Generation == n.Generation && prior.Epoch > n.Epoch {
				return nil, time.Time{}, errors.New("persistent recovery epoch rewound or log disappeared")
			}
		}
		for _, prior := range op.PersistentMembers {
			if prior.Node == n.Name && prior.Generation == n.Generation && prior.Epoch > n.Epoch {
				return nil, time.Time{}, errors.New("captured recovery epoch rewound or log disappeared")
			}
		}
		index := slices.IndexFunc(members, func(m persistentMember) bool { return m.Node == n.Name })
		donor := stopping && n.Name == op.TargetPod
		if index < 0 && !donor {
			if !slices.ContainsFunc(history, func(m persistentMember) bool {
				return m.Node == n.Name && m.Generation == n.Generation && resolvedMember(m)
			}) {
				return nil, time.Time{}, errors.New("unknown persistent storage writer")
			}
			if n.ExpiresMS > uint64(r.capacityNow().UnixMilli()) || (n.LogState != "" && n.LogState != "sealed") {
				return nil, time.Time{}, errors.New("retired persistent session revived or unsealed")
			}
			continue
		}
		var current persistentMember
		if index >= 0 {
			current = members[index]
		} else {
			ix := slices.IndexFunc(op.PersistentMembers, func(m persistentMember) bool { return m.Node == n.Name })
			if ix < 0 {
				return nil, time.Time{}, errors.New("missing donor capture")
			}
			current = op.PersistentMembers[ix]
		}
		if n.Generation != current.Generation {
			return nil, time.Time{}, errors.New("persistent runtime generation differs from launcher")
		}
		if donor {
			if !current.Stopped || n.ExpiresMS > uint64(r.capacityNow().UnixMilli()) || (n.LogState != "sealed" && n.LogState != "") {
				return nil, time.Time{}, errors.New("stopped donor recovery or lease expiry incomplete")
			}
		} else if n.ExpiresMS <= uint64(r.capacityNow().UnixMilli()) {
			return nil, time.Time{}, errors.New("persistent survivor lease expired")
		}
		if stopping && !donor && slices.Contains(n.Ensemble, op.TargetPod) {
			return nil, time.Time{}, errors.New("survivor still references departing follower; wait for runtime tiering and ensemble replacement")
		}
		if index >= 0 {
			members[index].Epoch = n.Epoch
			members[index].Ensemble = slices.Clone(n.Ensemble)
		}
	}
	// Positive direct metadata is required for every current invocation and donor.
	for _, m := range members {
		if !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool { return n.Name == m.Node && n.Generation == m.Generation }) {
			return nil, time.Time{}, errors.New("persistent node metadata missing")
		}
	}
	if issued && !slices.ContainsFunc(inventory.Nodes, func(n v050.Node) bool { return n.Name == op.TargetPod && n.Generation == op.TargetGeneration }) {
		return nil, time.Time{}, errors.New("retired donor metadata missing")
	}
	if stopping {
		for _, old := range op.PersistentMembers {
			if old.Node == op.TargetPod {
				continue
			}
			n := inventory.Nodes[slices.IndexFunc(inventory.Nodes, func(n v050.Node) bool { return n.Name == old.Node })]
			if n.Epoch < old.Epoch || (slices.Contains(old.Ensemble, op.TargetPod) && n.Epoch <= old.Epoch && n.LogState != "sealed") {
				return nil, time.Time{}, errors.New("departing follower obligations lack a tiering barrier")
			}
		}
	}
	policy := f.DeepCopy()
	if policy.Spec.Capacity == nil {
		policy.Spec.Capacity = &fleet.CapacityPolicy{}
		policy.Spec.Capacity.Default()
	}
	if r.Collector == nil {
		return nil, time.Time{}, errors.New("survivor metrics unavailable")
	}
	observation := r.Collector.Collect(ctx, policy)
	var ids []string
	var donorID string
	for _, m := range members {
		if stopping && m.Node == op.TargetPod {
			donorID = m.Container
			continue
		}
		ids = append(ids, m.Container)
	}
	if stopping {
		if donorID != "" {
			observation.Samples = slices.DeleteFunc(observation.Samples, func(s capacity.Sample) bool { return s.Identity == donorID })
		}
		if !bucketObservationIdentities(observation, ids) || !capacity.LowDemand(*policy.Spec.Capacity, observation, op.To) {
			return nil, time.Time{}, errors.New("persistent survivor health or capacity uncertain")
		}
	} else {
		donor := op.TargetPod
		if donor == "" {
			donor = fmt.Sprintf("%s-%d", f.Name, op.To)
		}
		donorIndex := slices.IndexFunc(members, func(m persistentMember) bool { return m.Node == donor })
		if donorIndex < 0 {
			return nil, time.Time{}, errors.New("highest ordinal donor missing")
		}
		if err := ValidateSurvivors(*policy.Spec.Capacity, observation, ids, members[donorIndex].Container, r.capacityNow()); err != nil {
			return nil, time.Time{}, err
		}
	}
	placement := map[types.UID]bucketCandidate{}
	for _, m := range members {
		donor := op.TargetPod
		if donor == "" {
			donor = fmt.Sprintf("%s-%d", f.Name, op.To)
		}
		if m.Node == donor {
			continue
		}
		placement[types.UID(m.PodUID)] = bucketCandidate{Host: m.Host, HostUID: m.HostUID, Hostname: m.Hostname, Zone: m.Zone}
	}
	if err := validateBucketPlacement(f, placement, false); err != nil {
		return nil, time.Time{}, err
	}
	after, err := r.persistentMembers(ctx, f, j, count, stopping)
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, m := range members {
		if !slices.ContainsFunc(after, func(p persistentMember) bool { return sameInvocation(m, p) && m.Stopped == p.Stopped }) {
			return nil, time.Time{}, errors.New("persistent membership changed during assessment")
		}
	}
	if inventory.ObservedAt.After(r.capacityNow()) || r.capacityNow().Sub(inventory.ObservedAt) > 5*time.Second || observation.At.After(r.capacityNow()) || r.capacityNow().Sub(observation.At) > capacity.Seconds(policy.Spec.Capacity.MaxAgeSeconds) {
		return nil, time.Time{}, errors.New("persistent assessment expired")
	}
	return members, inventory.ObservedAt, nil
}

func (r *Reconciler) contractPersistent(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	report := func(reason, message string) (ctrl.Result, bool, error) {
		result, err := r.report(ctx, f, reason, message, false)
		return result, true, err
	}
	save := func() (ctrl.Result, bool, error) {
		return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
	}
	fail := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		if !op.SettledAt.IsZero() {
			op.SettledAt = time.Time{}
			if e := r.saveJournal(ctx, res, j); e != nil {
				return ctrl.Result{}, true, e
			}
		}
		return report("PersistentRecoveryBlocked", err.Error())
	}
	if j.Loss != "" {
		return report("PossibleDataLoss", j.Loss)
	}
	if op.Phase == "Blocked" || op.Phase == "Intent" {
		if op.Stalled {
			return report("OperationStalled", "Persistent removal expired before graceful stop")
		}
		if maintenanceFence(f) != "" {
			return report("MaintenancePaused", "Persistent removal remains unissued")
		}
		if op.Automatic && !r.Options.LocalTest {
			return report("PersistentAutomaticUnqualified", "Automatic PersistentFleet contraction requires EKS/S3/EBS qualification")
		}
		if externalOwner(f) && !r.Options.LocalTest {
			return report("ExternalContractionUnqualified", "Contraction requested through /scale by an external writer awaits the same EKS/S3/EBS release qualification as Automatic mode; additions proceed")
		}
		if !op.Automatic && f.Spec.Replicas >= op.From {
			return report("DesiredChanged", "Persistent removal awaits matching intent or cancellation")
		}
		if op.To < 2 {
			return report("FollowerRetirementUnqualified", "Graceful PersistentFleet contraction currently retains at least two live nodes; last follower tiering lacks an external completion watermark")
		}
		members, _, err := r.assessPersistent(ctx, f, j, false, false)
		if err != nil {
			return fail(err)
		}
		if op.Phase == "Blocked" {
			op.PersistentMembers = members
			op.TargetPod = fmt.Sprintf("%s-%d", f.Name, op.To)
			for _, m := range members {
				if m.Node == op.TargetPod {
					op.TargetUID = m.PodUID
					op.TargetGeneration = m.Generation
				}
			}
			op.Phase = "Intent"
			return save()
		}
		for _, m := range members {
			if !slices.ContainsFunc(op.PersistentMembers, func(p persistentMember) bool { return sameInvocation(m, p) }) {
				return fail(errors.New("persistent capture changed before stop"))
			}
		}
		if op.Automatic {
			if f.Spec.Capacity == nil || f.Spec.Capacity.Mode != "Automatic" || j.Capacity == nil || op.PolicyHash != j.Capacity.Config || op.ManualBaseline != f.Spec.Replicas || j.Capacity.LowSince.IsZero() || j.Capacity.LowSamples < f.Spec.Capacity.MinSamples || r.capacityNow().Sub(j.Capacity.LowSince) < capacity.Seconds(f.Spec.Capacity.ScaleInStabilizationSeconds) {
				return report("CapacityChanged", "Fresh stable Automatic policy required")
			}
		}
		op.PersistentMembers = members // persist latest follower epochs before stopping.
		op.Phase = "Stopping"          // irreversible authority; cancellation no longer bypasses it.
		return save()
	}
	issued := replicas(w) == op.To && w.GetAnnotations()[operationKey] == op.ID
	if !issued && op.Phase == "Stopping" {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: op.TargetPod}, pod); err != nil {
			return fail(err)
		}
		id, _ := podIdentity(pod)
		oldIndex := slices.IndexFunc(op.PersistentMembers, func(m persistentMember) bool { return m.Node == op.TargetPod })
		if oldIndex < 0 {
			return fail(errors.New("missing exact donor admission"))
		}
		old := op.PersistentMembers[oldIndex]
		if string(pod.UID) != old.PodUID || id != old.Container {
			if !r.capacityNow().Before(op.Deadline.Add(stopExpiryGrace)) {
				// The captured donor is gone and every stop request for it has
				// expired; no Retiring authority was ever recorded. The removal is
				// unissued and cancels; the old generation stays unresolved history.
				op.Phase = "Canceling"
				return save()
			}
			return fail(errors.New("donor invocation changed before stop"))
		}
		state, err := r.callLauncher(ctx, f, pod, "", "")
		if err != nil || slices.ContainsFunc(j.InfrastructureFences, func(receipt infrastructureFence) bool {
			return receipt.Operation == op.ID && sameInvocation(receipt.Member, old)
		}) {
			fenced, fenceErr := r.ensureInfrastructureFence(ctx, f, res, j, old, op.ID)
			if fenceErr != nil {
				return fail(fenceErr)
			}
			if !fenced {
				return report("InfrastructureFencing", "Waiting for positive exact EC2 instance termination")
			}
			members, _, assessmentErr := r.assessPersistent(ctx, f, j, true, false)
			if assessmentErr != nil {
				return fail(assessmentErr)
			}
			op.PersistentMembers = members
			op.Phase = "Retiring"
			op.WorkloadVersion = w.GetResourceVersion()
			return save()
		}
		if state.Invocation != old.Invocation || state.Generation != old.Generation {
			return fail(errors.New("donor launcher changed before stop"))
		}
		if state.Phase == "Running" {
			if op.Automatic {
				if f.Spec.Capacity == nil || f.Spec.Capacity.Mode != "Automatic" || j.Capacity == nil || op.PolicyHash != j.Capacity.Config || op.ManualBaseline != f.Spec.Replicas || op.To < f.Spec.Capacity.MinReplicas {
					return report("CapacityChanged", "Automatic policy changed before graceful stop")
				}
				observation := r.Collector.Collect(ctx, f)
				*j.Capacity = capacity.Evaluate(*f.Spec.Capacity, *j.Capacity, observation, op.From)
				if err := r.saveJournal(ctx, res, j); err != nil {
					return ctrl.Result{}, true, err
				}
				if !capacity.LowDemand(*f.Spec.Capacity, observation, op.From) || j.Capacity.LowSince.IsZero() || j.Capacity.LowSamples < f.Spec.Capacity.MinSamples || observation.At.Sub(j.Capacity.LowSince) < capacity.Seconds(f.Spec.Capacity.ScaleInStabilizationSeconds) {
					return report("CapacityUncertain", "Fresh sustained low demand required before graceful stop")
				}
			}
			if op.Stalled || !r.capacityNow().Before(op.Deadline) {
				if !r.capacityNow().Before(op.Deadline.Add(stopExpiryGrace)) {
					// The launcher positively reports Running and every stop request for
					// this operation has expired: nothing was issued. Cancel through the
					// ordinary workload-CAS fence instead of holding the fleet forever.
					op.Phase = "Canceling"
					return save()
				}
				return report("OperationStalled", "No graceful stop issued before deadline; canceling once every stop request has provably expired")
			}
			members, _, err := r.assessPersistent(ctx, f, j, false, false)
			if err != nil {
				return fail(err)
			}
			for _, m := range members {
				if !slices.ContainsFunc(op.PersistentMembers, func(p persistentMember) bool { return sameInvocation(m, p) }) {
					return fail(errors.New("persistent invocation changed before stop request"))
				}
			}
			stopCtx, stopCancel := context.WithDeadline(ctx, op.Deadline)
			state, err = r.callLauncher(stopCtx, f, pod, op.ID, old.Generation)
			stopCancel()
			if err != nil {
				return fail(err)
			}
		}
		if state.Operation != op.ID {
			return fail(errors.New("launcher stop belongs to another operation"))
		}
		if state.Invocation != old.Invocation || state.Generation != old.Generation {
			return fail(errors.New("donor launcher invocation changed"))
		}
		if state.Phase != "Stopped" {
			return report("LifecycleProgress", "Waiting for launcher child exit and exclusive inherited-lock release")
		}
		members, _, err := r.assessPersistent(ctx, f, j, true, false)
		if err != nil {
			return fail(err)
		}
		op.PersistentMembers = members
		op.Phase = "Retiring"
		op.WorkloadVersion = w.GetResourceVersion()
		return save()
	}
	if !issued && op.Phase == "Retiring" {
		if _, _, err := r.assessPersistent(ctx, f, j, true, false); err != nil {
			return fail(err)
		}
		// Stop is already issued. A deadline cannot strand a positively terminated
		// donor by preventing the cleanup decrement; stale generation CAS still applies.
		if w.GetResourceVersion() != op.WorkloadVersion {
			op.WorkloadVersion = w.GetResourceVersion()
			return save()
		}
		cleanup := *op
		cleanup.Deadline = time.Time{}
		if err := r.applyReplicas(ctx, w, &cleanup); err != nil {
			return fail(err)
		}
		op.Phase = "Recovering"
		return save()
	}
	if !issued {
		return fail(errors.New("persistent operation authority differs from workload"))
	}
	if op.Phase != "Recovering" {
		op.Phase = "Recovering"
		return save()
	}
	members, at, err := r.assessPersistent(ctx, f, j, true, true)
	if err != nil {
		return fail(err)
	}
	if op.SettledAt.IsZero() {
		op.SettledAt = at
		return save()
	}
	if at.Sub(op.SettledAt) < 10*time.Second {
		return report("LifecycleProgress", "Revalidating PersistentFleet recovery and survivor settling")
	}
	for _, old := range op.PersistentMembers {
		if old.Node == op.TargetPod {
			old.Retired = true
			old.Stopped = true
			members = append(members, old)
		}
	}
	for _, m := range members {
		ix := slices.IndexFunc(j.PersistentHistory, func(p persistentMember) bool { return p.Node == m.Node && p.Generation == m.Generation })
		if ix < 0 {
			j.PersistentHistory = append(j.PersistentHistory, m)
		} else {
			j.PersistentHistory[ix] = m
		}
	}
	done := completion(op, at)
	done.Outcome = "LauncherStoppedRecoveredFollowerRetired"
	j.History = append(j.History, done)
	j.Applied, j.Operation = op.To, nil
	if j.Capacity != nil {
		capacity.RecordAction(j.Capacity, r.capacityNow(), op.From, op.To)
	}
	return save()
}

func reactivatedNode(name, node string, from, to int32) bool {
	for n := from; n < to; n++ {
		if node == fmt.Sprintf("%s-%d", name, n) {
			return true
		}
	}
	return false
}
func (r *Reconciler) finishReactivation(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation, j *lifecycleJournal, w client.Object) (ctrl.Result, bool, error) {
	op := j.Operation
	fail := func(err error) (ctrl.Result, bool, error) {
		if _, ok := errors.AsType[*v050.LossError](err); ok {
			return r.recordLoss(ctx, f, w, res, j, err.Error())
		}
		result, e := r.report(ctx, f, "ReactivationBlocked", err.Error(), false)
		return result, true, e
	}
	if replicas(w) != op.To || w.GetAnnotations()[operationKey] != op.ID {
		return fail(errors.New("reactivation replica authority changed"))
	}
	members, err := r.persistentMembers(ctx, f, j, op.To, false)
	if err != nil {
		return fail(err)
	}
	reader, err := r.Evidence.reader(ctx, f)
	if err != nil {
		return fail(err)
	}
	adapter, err := catalog.New(runtimeImage(evidenceRuntime(f, j)))
	if err != nil {
		return fail(err)
	}
	inventory, err := adapter.Inventory(ctx, reader, r.capacityNow)
	if err != nil {
		return fail(err)
	}
	for i := range members {
		m := &members[i]
		index := slices.IndexFunc(inventory.Nodes, func(n v050.Node) bool {
			return n.Name == m.Node && n.Generation == m.Generation && n.ExpiresMS > uint64(r.capacityNow().UnixMilli())
		})
		if index < 0 {
			return fail(errors.New("reactivated launcher generation has no live node record"))
		}
		m.Epoch = inventory.Nodes[index].Epoch
		m.Ensemble = slices.Clone(inventory.Nodes[index].Ensemble)
		for _, prior := range j.PersistentHistory {
			if prior.Node != m.Node {
				continue
			}
			if reactivatedNode(f.Name, m.Node, op.From, op.To) {
				if !prior.Retired || !prior.Stopped || !prior.RestartDenied || prior.Generation == m.Generation || prior.ClaimUID != m.ClaimUID || prior.VolumeUID != m.VolumeUID || prior.VolumeHandle != m.VolumeHandle {
					return fail(errors.New("reactivation lacks retired predecessor or unchanged volume identity"))
				}
				latest, _ := latestPersistentMember(j.PersistentHistory, m.Node)
				if prior.Generation == latest.Generation && (prior.Host != m.Host || prior.HostUID != m.HostUID || prior.BootID != m.BootID) {
					if prior.DiskID == "" || prior.DiskID != m.DiskID || prior.Zone == "" || prior.Zone != m.Zone || r.Options.LocalTest {
						return fail(errors.New("cross-host reactivation lacks authenticated disk and zone continuity"))
					}
					if err := r.verifyVolumeAttachment(ctx, f, *m); err != nil {
						return fail(err)
					}
				}
			} else if !prior.Retired && !prior.Superseded && !sameInvocation(prior, *m) {
				return fail(errors.New("survivor changed during reactivation"))
			}
		}
	}
	after, err := r.persistentMembers(ctx, f, j, op.To, false)
	if err != nil {
		return fail(err)
	}
	for _, m := range members {
		if !slices.ContainsFunc(after, func(current persistentMember) bool { return sameInvocation(m, current) }) {
			return fail(errors.New("runtime changed while observing reactivation evidence"))
		}
	}
	if inventory.ObservedAt.After(r.capacityNow()) || r.capacityNow().Sub(inventory.ObservedAt) > 5*time.Second {
		return fail(errors.New("reactivation evidence expired"))
	}
	for _, m := range members {
		if !slices.ContainsFunc(j.PersistentHistory, func(old persistentMember) bool { return old.Node == m.Node && old.Generation == m.Generation }) {
			j.PersistentHistory = append(j.PersistentHistory, m)
		}
	}
	done := completion(op, inventory.ObservedAt)
	done.Outcome = "RetainedVolumeReactivated"
	j.History = append(j.History, done)
	j.Applied, j.Operation = op.To, nil
	return ctrl.Result{RequeueAfter: time.Second}, true, r.saveJournal(ctx, res, j)
}

func validatePersistentJournal(j *lifecycleJournal) error {
	groups := [][]persistentMember{j.PersistentHistory}
	seenFences := map[string]bool{}
	for _, receipt := range j.InfrastructureFences {
		key := receipt.Operation + "/" + receipt.Member.Node + "/" + receipt.Member.Generation
		if err := validInfrastructureReceipt(receipt); err != nil {
			return err
		}
		if seenFences[key] {
			return errors.New("duplicate infrastructure fencing receipt")
		}
		seenFences[key] = true
		groups = append(groups, []persistentMember{receipt.Member})
	}
	if j.Operation != nil {
		groups = append(groups, j.Operation.PersistentMembers)
	}
	for _, group := range groups {
		seen := map[string]bool{}
		for _, m := range group {
			key := m.Node + "/" + m.Generation
			if m.Node == "" || m.PodUID == "" || m.Container == "" || m.Host == "" || m.HostUID == "" || m.Hostname == "" || m.BootID == "" || m.Invocation == "" || m.Generation == "" || m.ClaimUID == "" || m.VolumeUID == "" || m.VolumeHandle == "" || string(j.Claims["data-"+m.Node]) != m.ClaimUID || (m.Retired && !m.Stopped) || (m.RestartDenied && !m.Stopped) || seen[key] {
				return errors.New("invalid PersistentFleet invocation authority")
			}
			seen[key] = true
		}
	}
	return nil
}

func (r *Reconciler) persistentVolumeIdentity(ctx context.Context, claim *corev1.PersistentVolumeClaim) (string, string, error) {
	if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
		return "", "", errors.New("persistent claim is not positively bound")
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, pv); err != nil {
		return "", "", err
	}
	ref := pv.Spec.ClaimRef
	if pv.UID == "" || !pv.DeletionTimestamp.IsZero() || ref == nil || ref.UID != claim.UID || ref.Name != claim.Name || ref.Namespace != claim.Namespace || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return "", "", errors.New("persistent volume binding or retention identity changed")
	}
	if r.Options.LocalTest && pv.Spec.HostPath != nil && pv.Spec.HostPath.Path != "" {
		return string(pv.UID), "local-hostPath:" + pv.Spec.HostPath.Path, nil
	}
	if r.Options.LocalTest && pv.Spec.CSI != nil && pv.Spec.CSI.Driver == localCSIDriver && pv.Spec.CSI.VolumeHandle != "" {
		// Local per-node CSI volumes: real CSI handles and ReadWriteOncePod, no EBS.
		return string(pv.UID), localCSIDriver + ":" + pv.Spec.CSI.VolumeHandle, nil
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "ebs.csi.aws.com" || pv.Spec.CSI.VolumeHandle == "" {
		return "", "", errors.New("qualified EBS CSI volume identity unavailable")
	}
	return string(pv.UID), pv.Spec.CSI.VolumeHandle, nil
}
