package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/fencing"
	"github.com/ewhauser/celld-operator/internal/launcher"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recoveryFixture is a production-shaped three-member PersistentFleet on
// dedicated EC2-backed nodes with EBS CSI ReadWriteOncePod volumes, steady
// state, no operation in flight, and a fake EC2 API for the exact instances.
type recoveryFixture struct {
	*persistentFixture
	api *fakeInfrastructure
}

func instanceID(i int) string { return fmt.Sprintf("i-%017d", i) }
func volumeID(i int) string   { return fmt.Sprintf("vol-%017d", i) }

func recoverySetup(t *testing.T) *recoveryFixture {
	t.Helper()
	f := fixture("persistent", "persistent-data", "PersistentFleet")
	f.Spec.Replicas = 3
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	opts := Options{LauncherImage: "registry.example/celld-operator@sha256:" + strings.Repeat("a", 64), OperatorNamespace: "celld-system", FencingAccount: "123456789012", FencingRegion: "us-east-1"}
	reader := &persistentReader{now: time.Unix(10000, 0), generation: map[string]string{}}
	w := workload(f, opts).(*appsv1.StatefulSet)
	w.UID = "workload"
	w.Annotations = map[string]string{}
	j := &lifecycleJournal{Version: 9, RuntimeImage: Image, Initial: 3, Applied: 3, WorkloadUID: w.UID, Claims: map[string]types.UID{}}
	class := &storagev1.StorageClass{Name: "gp3", Provisioner: "ebs.csi.aws.com", Parameters: map[string]string{"type": "gp3"}, VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer), ReclaimPolicy: new(corev1.PersistentVolumeReclaimRetain)}
	objects := []client.Object{f, w, class}
	states := map[string]launcher.State{}
	for i := range 3 {
		name := fmt.Sprintf("persistent-%d", i)
		host := fmt.Sprintf("host-%d", i)
		gen := fmt.Sprintf("generation-%d", i)
		reader.generation[name] = gen
		pod := &corev1.Pod{Name: name, Namespace: f.Namespace, UID: types.UID("uid-" + name), Labels: labels(f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: f.Name, UID: w.UID, Controller: new(true)}}, Spec: *w.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.0.0.%d", i+1), Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(reader.now.Add(-time.Minute))}}}}}}
		pod.Spec.NodeName = host
		pod.Spec.SchedulingGates = nil
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + name}})
		claim := &corev1.PersistentVolumeClaim{Name: "data-" + name, Namespace: f.Namespace, UID: types.UID("claim-" + name), Labels: labels(f), Annotations: map[string]string{"celld.eric.dev/storage-reservation": reservationName(f)}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, StorageClassName: new("gp3"), VolumeName: "pv-" + name}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
		pv := &corev1.PersistentVolume{Name: claim.Spec.VolumeName, UID: types.UID("pvuid-" + name), Spec: corev1.PersistentVolumeSpec{StorageClassName: "gp3", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: volumeID(i)}}}}
		j.Claims[claim.Name] = claim.UID
		id, _ := podIdentity(pod)
		j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: name, Generation: gen, Container: id, Current: true, Epoch: 1})
		node := &corev1.Node{Name: host, UID: types.UID(host), Labels: map[string]string{corev1.LabelHostname: host, corev1.LabelTopologyZone: "us-east-1a"}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/" + instanceID(i)}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		states[name] = launcher.State{PodUID: string(pod.UID), Node: name, Host: host, Invocation: "invocation-" + name, Generation: gen, Phase: "Running", BootID: "boot", DiskID: "disk-" + name}
		objects = append(objects, pod, node, claim, pv)
	}
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	res.Annotations = map[string]string{attemptAnnotation: "created"}
	objects = append(objects, res)
	r := setup(t, objects...)
	r.Options = opts
	r.now = func() time.Time { return reader.now }
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return reader, nil }}
	r.launcherCall = func(_ context.Context, _ *fleet.CelldFleet, p *corev1.Pod, op, gen string) (launcher.State, error) {
		s, ok := states[p.Name]
		if !ok || s.PodUID != string(p.UID) {
			return launcher.State{}, errors.New("launcher unreachable")
		}
		return s, nil
	}
	api := &fakeInfrastructure{instance: fencing.Instance{Account: "123456789012", ID: instanceID(2), Zone: "us-east-1a", State: "running", Tags: map[string]string{fencing.FleetTag: string(f.UID), fencing.HostTag: "host-2", fencing.BootTag: "boot", fencing.FenceTag: "terminate"}, Disks: []fencing.Disk{{ID: volumeID(2)}}}}
	r.Infrastructure = api
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	o := capacity.Observation{At: reader.now, Complete: true}
	r.Collector = bucketCapacityCollector{o}
	x := &recoveryFixture{persistentFixture: &persistentFixture{r: r, f: f, j: j, res: res, w: w, reader: reader, states: states}, api: api}
	// Steady state admits every running invocation before any failure.
	x.reconcile(t)
	if len(x.j.PersistentHistory) != 3 {
		t.Fatalf("steady-state admission recorded %d members", len(x.j.PersistentHistory))
	}
	return x
}

// reconcile runs one cold lifecycle pass with fresh fleet, journal and workload reads.
func (x *recoveryFixture) reconcile(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.f), x.f); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.res), x.res); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.w), x.w); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.r.lifecycle(ctx, x.f, x.res, x.w); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.res), x.res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(x.res)
	if err != nil {
		t.Fatal(err)
	}
	x.j = j
	return readyReason(t, x.r, x.f)
}

// member returns the latest admitted invocation of the ordinal every scenario loses.
func (x *recoveryFixture) member(t *testing.T) persistentMember {
	t.Helper()
	m, ok := latestPersistentMember(x.j.PersistentHistory, "persistent-2")
	if !ok {
		t.Fatal("persistent-2 not admitted")
	}
	return m
}

func (x *recoveryFixture) annotate(t *testing.T, value string) {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	if value == "" {
		delete(got.Annotations, fenceRequestKey)
	} else {
		got.Annotations[fenceRequestKey] = value
	}
	if err := x.r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
}

// loseHost simulates an instance that vanished under Kubernetes: the Node
// registration and its pods are garbage collected, and the StatefulSet has
// recreated the ordinal behind the scheduling gate.
func (x *recoveryFixture) loseHost(t *testing.T) *corev1.Pod {
	t.Helper()
	ctx := t.Context()
	name := "persistent-2"
	if err := x.r.Delete(ctx, &corev1.Node{Name: "host-2"}); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(ctx, &corev1.Pod{Name: name, Namespace: x.f.Namespace}); err != nil {
		t.Fatal(err)
	}
	gated := &corev1.Pod{Name: name, Namespace: x.f.Namespace, UID: types.UID(name + "-b"), Labels: labels(x.f), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: x.w.UID, Controller: new(true)}}, Spec: *x.w.Spec.Template.Spec.DeepCopy()}
	gated.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	gated.Spec.NodeName = ""
	if err := x.r.Create(ctx, gated); err != nil {
		t.Fatal(err)
	}
	return gated
}

// startReplacement stands in for the scheduler, kubelet and the granted
// launcher: the gated pod lands on a fresh node in the zone, the EBS volume is
// attached exclusively there, celld runs with a new generation and its lease
// record replaces the retired one.
func (x *recoveryFixture) startReplacement(t *testing.T, pod *corev1.Pod, host, generation string) {
	t.Helper()
	ctx := t.Context()
	node := &corev1.Node{Name: host, UID: types.UID(host), Labels: map[string]string{corev1.LabelHostname: host, corev1.LabelTopologyZone: "us-east-1a"}, Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/" + instanceID(9)}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-new"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if err := x.r.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = host
	pod.Spec.SchedulingGates = nil
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + pod.Name}})
	if err := x.r.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{PodIP: "10.0.0.9", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(x.reader.now)}}}}}
	if err := x.r.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	attachment := &storagev1.VolumeAttachment{Name: "attach-" + pod.Name, Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: host, Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: new("pv-" + pod.Name)}}, Status: storagev1.VolumeAttachmentStatus{Attached: true}}
	if err := x.r.Create(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	x.states[pod.Name] = launcher.State{PodUID: string(pod.UID), Node: pod.Name, Host: host, Invocation: "invocation-b", Generation: generation, Phase: "Running", BootID: "boot-new", DiskID: "disk-" + pod.Name, PreviousHost: "host-2\nboot"}
	x.reader.generation[pod.Name] = generation
	x.reader.stopped = false
}

func TestRecoveryReportsUnreachableMemberWithoutActing(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	gated := x.loseHost(t)
	for range 2 {
		if got := x.reconcile(t); got != "PersistentMemberUncertain" {
			t.Fatalf("expected PersistentMemberUncertain, got %s", got)
		}
	}
	if x.j.Recovery != nil || len(x.api.terminations) != 0 || len(x.j.InfrastructureFences) != 0 {
		t.Fatalf("report acted: %+v terminations=%v", x.j.Recovery, x.api.terminations)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(gated), gated); err != nil {
		t.Fatal(err)
	}
	if len(gated.Spec.SchedulingGates) != 1 {
		t.Fatal("gate released for an unresolved writer")
	}
	got := &fleet.CelldFleet{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" && !strings.Contains(c.Message, fenceRequestKey+"="+recoveryID(old)) {
			t.Fatalf("message does not name the exact authorization: %s", c.Message)
		}
	}
	if m := x.member(t); !sameInvocation(m, old) || resolvedMember(m) {
		t.Fatal("history changed without evidence")
	}
}

func TestRecoveryIgnoresAuthorizationForReachableMember(t *testing.T) {
	x := recoverySetup(t)
	x.annotate(t, recoveryID(x.member(t)))
	x.reconcile(t)
	if x.j.Recovery != nil || len(x.api.terminations) != 0 {
		t.Fatal("healthy member was scheduled for fencing")
	}
}

func TestRecoveryAcceptsAlreadyTerminatedExactInstanceAndReactivatesDisk(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	gated := x.loseHost(t)
	x.api.instance.State = "terminated"
	x.api.instance.Disks = nil
	x.annotate(t, recoveryID(old))
	if got := x.reconcile(t); got != "LifecycleProgress" || x.j.Recovery == nil || x.j.Recovery.Phase != "Fencing" {
		t.Fatalf("recovery record not created: %s %+v", got, x.j.Recovery)
	}
	x.reconcile(t)
	if x.j.Recovery == nil || x.j.Recovery.Phase != "Reactivating" || len(x.api.terminations) != 0 {
		t.Fatalf("terminated instance not accepted as receipt: %+v terminations=%v", x.j.Recovery, x.api.terminations)
	}
	if !certifiedInfrastructureFence(x.j, old) {
		t.Fatal("receipt not durable")
	}
	retired := x.member(t)
	if !retired.Stopped || !retired.RestartDenied || !retired.Retired || retired.Epoch != 1 {
		t.Fatalf("fenced invocation not retired: %+v", retired)
	}
	// Reactivating: the gate opens with a zone pin, never a host pin.
	if got := x.reconcile(t); got != "PersistentRecoveryBlocked" {
		t.Fatalf("expected to wait for the replacement, got %s", got)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(gated), gated); err != nil {
		t.Fatal(err)
	}
	if len(gated.Spec.SchedulingGates) != 0 || gated.Spec.NodeSelector[corev1.LabelTopologyZone] != "us-east-1a" || gated.Spec.NodeSelector[corev1.LabelHostname] != "" {
		t.Fatalf("unexpected scheduling constraints: %+v", gated.Spec)
	}
	x.startReplacement(t, gated, "host-9", "generation-2b")
	if got := x.reconcile(t); got != "LifecycleProgress" || x.j.Recovery != nil {
		t.Fatalf("recovery did not complete: %s %+v", got, x.j.Recovery)
	}
	replacement := x.member(t)
	if replacement.Generation != "generation-2b" || replacement.Host != "host-9" || replacement.PodUID != "persistent-2-b" || replacement.VolumeHandle != old.VolumeHandle || replacement.DiskID != old.DiskID {
		t.Fatalf("replacement not admitted: %+v", replacement)
	}
	last := x.j.History[len(x.j.History)-1]
	if last.Outcome != "InfrastructureFencedMemberReactivated" || last.TargetGeneration != old.Generation || last.From != 3 || last.To != 3 {
		t.Fatalf("unexpected completion %+v", last)
	}
	if x.j.Applied != 3 || len(x.j.PersistentHistory) != 4 {
		t.Fatalf("authority changed: applied=%d history=%d", x.j.Applied, len(x.j.PersistentHistory))
	}
	x.annotate(t, "")
	if got := x.reconcile(t); got == "PersistentMemberUncertain" || got == "PersistentRecoveryBlocked" {
		t.Fatalf("fleet stays blocked after recovery: %s", got)
	}
}

func TestRecoveryTerminatesRunningInstanceOnlyAfterDurableIntentAndCordon(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	ctx := t.Context()
	// Kubelet stopped reporting: NotReady node, pod still present, launcher gone.
	node := &corev1.Node{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: "host-2"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
	if err := x.r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	delete(x.states, "persistent-2")
	if got := x.reconcile(t); got != "PersistentMemberUncertain" {
		t.Fatalf("expected PersistentMemberUncertain, got %s", got)
	}
	x.annotate(t, recoveryID(old))
	x.reconcile(t) // record
	if x.j.Recovery == nil || x.j.Recovery.Phase != "Fencing" {
		t.Fatalf("no recovery record: %+v", x.j.Recovery)
	}
	x.reconcile(t) // durable intent, nothing issued
	if len(x.j.InfrastructureFences) != 1 || !x.j.InfrastructureFences[0].ConfirmedAt.IsZero() || len(x.api.terminations) != 0 {
		t.Fatalf("intent not recorded before effect: %+v terminations=%v", x.j.InfrastructureFences, x.api.terminations)
	}
	// Withdrawing the annotation after intent does not withdraw the request.
	x.annotate(t, "")
	if got := x.reconcile(t); got != "PersistentRecoveryBlocked" || x.j.Recovery == nil {
		t.Fatalf("issued intent was discarded: %s %+v", got, x.j.Recovery)
	}
	x.annotate(t, recoveryID(old))
	x.reconcile(t) // cordon
	if err := x.r.Get(ctx, client.ObjectKey{Name: "host-2"}, node); err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable || len(x.api.terminations) != 0 {
		t.Fatal("termination issued before cordon")
	}
	if got := x.reconcile(t); got != "InfrastructureFencing" || len(x.api.terminations) != 1 || x.api.terminations[0] != instanceID(2) {
		t.Fatalf("exact termination not issued: %s %v", got, x.api.terminations)
	}
	x.api.instance.State = "shutting-down"
	if got := x.reconcile(t); got != "InfrastructureFencing" || x.j.Recovery.Phase != "Fencing" {
		t.Fatalf("shutting-down accepted as proof: %s %+v", got, x.j.Recovery)
	}
	x.api.instance.State = "terminated"
	x.api.instance.Disks = nil
	x.reconcile(t)
	if x.j.Recovery == nil || x.j.Recovery.Phase != "Reactivating" || len(x.api.terminations) != 1 {
		t.Fatalf("positive receipt not applied: %+v", x.j.Recovery)
	}
	// The dead pod is removed with the UID precondition so the ordinal can be recreated.
	x.reconcile(t)
	pod := &corev1.Pod{}
	if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "persistent-2"}, pod); err == nil {
		t.Fatalf("pod of terminated instance retained: %s", pod.UID)
	}
}

func TestRecoveryWithdrawsBeforeIntentAndRefusesReachableHost(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	ctx := t.Context()
	node := &corev1.Node{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: "host-2"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if err := x.r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	state := x.states["persistent-2"]
	delete(x.states, "persistent-2")
	x.annotate(t, recoveryID(old))
	x.reconcile(t)
	if x.j.Recovery == nil {
		t.Fatal("no recovery record")
	}
	// The launcher answers again before any intent: no host may be terminated.
	x.states["persistent-2"] = state
	if got := x.reconcile(t); got != "PersistentRecoveryBlocked" || len(x.j.InfrastructureFences) != 0 || len(x.api.terminations) != 0 {
		t.Fatalf("reachable host fenced: %s fences=%d terminations=%v", got, len(x.j.InfrastructureFences), x.api.terminations)
	}
	x.annotate(t, "")
	if got := x.reconcile(t); got != "LifecycleProgress" || x.j.Recovery != nil {
		t.Fatalf("unissued request not withdrawn: %s %+v", got, x.j.Recovery)
	}
	if err := x.r.Get(ctx, client.ObjectKey{Name: "host-2"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := x.r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if got := x.reconcile(t); got == "PersistentMemberUncertain" {
		t.Fatal("recovered host still reported uncertain")
	}
}

func TestRecoveryRefusesUnsafeFencing(t *testing.T) {
	for _, fault := range []string{"disabled", "other-workload", "other-instance", "in-flight", "loss", "paused"} {
		t.Run(fault, func(t *testing.T) {
			x := recoverySetup(t)
			old := x.member(t)
			x.loseHost(t)
			ctx := t.Context()
			switch fault {
			case "disabled":
				x.r.Infrastructure = nil
			case "other-workload":
				// The registration was re-created under the same name and hosts another pod.
				if err := x.r.Create(ctx, &corev1.Node{Name: "host-2", UID: "host-2-again", Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/" + instanceID(2)}, Labels: map[string]string{corev1.LabelTopologyZone: "us-east-1a"}}); err != nil {
					t.Fatal(err)
				}
				if err := x.r.Create(ctx, &corev1.Pod{Name: "innocent", Namespace: "other", Spec: corev1.PodSpec{NodeName: "host-2"}}); err != nil {
					t.Fatal(err)
				}
			case "other-instance":
				x.api.instance.ID = instanceID(7)
			case "in-flight":
				x.j.Operation = &lifecycleOperation{ID: "add", Phase: "Intent", From: 3, To: 4, StartedAt: x.reader.now, Deadline: x.reader.now.Add(operationBudget)}
				if err := x.r.saveJournal(ctx, x.res, x.j); err != nil {
					t.Fatal(err)
				}
			case "loss":
				x.j.Loss = "durable loss"
				if err := x.r.saveJournal(ctx, x.res, x.j); err != nil {
					t.Fatal(err)
				}
			case "paused":
				got := &fleet.CelldFleet{}
				if err := x.r.Get(ctx, client.ObjectKeyFromObject(x.f), got); err != nil {
					t.Fatal(err)
				}
				got.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
				if err := x.r.Update(ctx, got); err != nil {
					t.Fatal(err)
				}
			}
			x.annotate(t, recoveryID(old))
			for range 4 {
				x.reconcile(t)
			}
			if len(x.api.terminations) != 0 || certifiedInfrastructureFence(x.j, old) {
				t.Fatalf("unsafe fence: terminations=%v", x.api.terminations)
			}
			if m := x.member(t); resolvedMember(m) {
				t.Fatal("history retired without a fence")
			}
		})
	}
}

func TestRecoveryReplacementMustKeepDiskZoneAndChangeHost(t *testing.T) {
	for _, fault := range []string{"disk", "zone", "volume", "attachment", "revived"} {
		t.Run(fault, func(t *testing.T) {
			x := recoverySetup(t)
			old := x.member(t)
			gated := x.loseHost(t)
			x.api.instance.State = "terminated"
			x.api.instance.Disks = nil
			x.annotate(t, recoveryID(old))
			x.reconcile(t)
			x.reconcile(t)
			x.reconcile(t)
			x.startReplacement(t, gated, "host-9", "generation-2b")
			ctx := t.Context()
			switch fault {
			case "disk":
				s := x.states["persistent-2"]
				s.DiskID = "copied"
				x.states["persistent-2"] = s
			case "zone":
				n := &corev1.Node{}
				if err := x.r.Get(ctx, client.ObjectKey{Name: "host-9"}, n); err != nil {
					t.Fatal(err)
				}
				n.Labels[corev1.LabelTopologyZone] = "us-east-1b"
				if err := x.r.Update(ctx, n); err != nil {
					t.Fatal(err)
				}
			case "volume":
				pv := &corev1.PersistentVolume{}
				if err := x.r.Get(ctx, client.ObjectKey{Name: "pv-persistent-2"}, pv); err != nil {
					t.Fatal(err)
				}
				pv.Spec.CSI.VolumeHandle = volumeID(8)
				if err := x.r.Update(ctx, pv); err != nil {
					t.Fatal(err)
				}
			case "attachment":
				if err := x.r.Create(ctx, &storagev1.VolumeAttachment{Name: "stale", Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: "host-2", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: new("pv-persistent-2")}}, Status: storagev1.VolumeAttachmentStatus{Attached: true}}); err != nil {
					t.Fatal(err)
				}
			case "revived":
				x.reader.generation["persistent-2"] = old.Generation
			}
			if got := x.reconcile(t); got != "PersistentRecoveryBlocked" || x.j.Recovery == nil {
				t.Fatalf("%s admitted: %s", fault, got)
			}
			if m := x.member(t); m.Generation == "generation-2b" {
				t.Fatal("replacement admitted into history")
			}
		})
	}
}

func TestRecoveryHandoffGrantRequiresReactivatingRecord(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	gated := x.loseHost(t)
	x.api.instance.State = "terminated"
	x.api.instance.Disks = nil
	x.annotate(t, recoveryID(old))
	x.reconcile(t)
	x.reconcile(t)
	if x.j.Recovery == nil || x.j.Recovery.Phase != "Reactivating" {
		t.Fatalf("not reactivating: %+v", x.j.Recovery)
	}
	x.startReplacement(t, gated, "host-9", "generation-2b")
	// The successor waits for the signed grant; the predecessor is expired and sealed.
	s := x.states["persistent-2"]
	s.Phase = "WaitingForHandoff"
	x.states["persistent-2"] = s
	x.reader.generation["persistent-2"] = old.Generation
	x.reader.stopped = true
	pod := &corev1.Pod{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(gated), pod); err != nil {
		t.Fatal(err)
	}
	err := x.r.authorizeVolumeHandoff(t.Context(), x.f, x.j, pod)
	if err == nil || strings.Contains(err.Error(), "reactivation authority") || strings.Contains(err.Error(), "predecessor") || strings.Contains(err.Error(), "attachment") {
		t.Fatalf("recovery authority not honored before the grant: %v", err)
	}
	withdrawn := *x.j
	withdrawn.Recovery = nil
	if err := x.r.authorizeVolumeHandoff(t.Context(), x.f, &withdrawn, pod); err == nil || !strings.Contains(err.Error(), "reactivation authority") {
		t.Fatalf("grant possible without a durable recovery record: %v", err)
	}
	pending := *x.j
	pending.Recovery = &persistentRecovery{ID: x.j.Recovery.ID, Member: x.j.Recovery.Member, Phase: "Fencing", StartedAt: x.j.Recovery.StartedAt}
	if err := x.r.authorizeVolumeHandoff(t.Context(), x.f, &pending, pod); err == nil || !strings.Contains(err.Error(), "reactivation authority") {
		t.Fatalf("grant possible before the fence receipt: %v", err)
	}
}

func TestRecoveryJournalValidation(t *testing.T) {
	x := recoverySetup(t)
	old := x.member(t)
	base := func() *lifecycleJournal {
		j := *x.j
		j.PersistentHistory = slices.Clone(x.j.PersistentHistory)
		j.Recovery = &persistentRecovery{ID: recoveryID(old), Member: old, Phase: "Fencing", StartedAt: x.reader.now}
		return &j
	}
	for name, mutate := range map[string]func(*lifecycleJournal){
		"valid":      func(*lifecycleJournal) {},
		"wrong-id":   func(j *lifecycleJournal) { j.Recovery.ID = "recover:persistent-2:other" },
		"not-latest": func(j *lifecycleJournal) { j.Recovery.Member.Generation = "older" },
		"bad-phase":  func(j *lifecycleJournal) { j.Recovery.Phase = "Uncertain" },
		"no-disk":    func(j *lifecycleJournal) { j.Recovery.Member.DiskID = "" },
		"reactivate-no-fence": func(j *lifecycleJournal) {
			j.Recovery.Phase = "Reactivating"
			i := slices.IndexFunc(j.PersistentHistory, func(m persistentMember) bool { return sameInvocation(m, old) })
			j.PersistentHistory[i].Stopped, j.PersistentHistory[i].RestartDenied, j.PersistentHistory[i].Retired = true, true, true
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := base()
			mutate(j)
			err := validatePersistentRecovery(j)
			if (err == nil) != (name == "valid") {
				t.Fatalf("validation %v", err)
			}
		})
	}
}

func TestSteadyStateAdmissionBlocksOnUnresolvedPredecessor(t *testing.T) {
	x := recoverySetup(t)
	// A new generation appears on persistent-1 without any gate or supersession.
	s := x.states["persistent-1"]
	s.Generation, s.Invocation = "generation-1-rogue", "invocation-rogue"
	x.states["persistent-1"] = s
	x.reader.generation["persistent-1"] = s.Generation
	if got := x.reconcile(t); got != "PersistentAdmissionBlocked" {
		t.Fatalf("expected PersistentAdmissionBlocked, got %s", got)
	}
	if slices.ContainsFunc(x.j.PersistentHistory, func(m persistentMember) bool { return m.Generation == "generation-1-rogue" }) {
		t.Fatal("unexplained successor admitted")
	}
}

func TestSteadyStateAdmissionSkipsWhileOperationInFlight(t *testing.T) {
	x := recoverySetup(t)
	x.j.PersistentHistory = nil
	x.j.Operation = &lifecycleOperation{ID: "add", Phase: "Intent", From: 3, To: 4, StartedAt: x.reader.now, Deadline: x.reader.now.Add(operationBudget)}
	if err := x.r.saveJournal(t.Context(), x.res, x.j); err != nil {
		t.Fatal(err)
	}
	x.reconcile(t)
	if len(x.j.PersistentHistory) != 0 {
		t.Fatal("admission ran during an operation")
	}
}
