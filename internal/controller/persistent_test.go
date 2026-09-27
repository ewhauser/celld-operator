package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// PersistentFleet (ADR 0024) is a StatefulSet the operator renders and
// applies. The StatefulSet controller restarts members; the operator deletes a
// Pod or a claim only to heal a member that cannot come back, and stores
// nothing.

func persistentFleet(t *testing.T) *operationFixture {
	t.Helper()
	x := newOperationFixture(t, "PersistentFleet")
	x.converge()
	return x
}

func (x *operationFixture) claimUIDs() map[string]types.UID {
	x.t.Helper()
	list := &corev1.PersistentVolumeClaimList{}
	if err := x.r.List(x.t.Context(), list, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := map[string]types.UID{}
	for _, c := range list.Items {
		if c.DeletionTimestamp.IsZero() {
			out[c.Name] = c.UID
		}
	}
	return out
}

func (x *operationFixture) podUIDs() map[string]types.UID {
	x.t.Helper()
	list := &corev1.PodList{}
	if err := x.r.List(x.t.Context(), list, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := map[string]types.UID{}
	for _, p := range list.Items {
		out[p.Name] = p.UID
	}
	return out
}

// operatorStep reconciles once and fails the test if the operator deletes a
// Pod or a claim. The simulated workload controller is not wrapped.
func (x *operationFixture) operatorStep() *fleet.CelldFleet {
	x.t.Helper()
	base := x.r.Client
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			switch obj.(type) {
			case *corev1.Pod, *corev1.PersistentVolumeClaim:
				x.t.Fatalf("operator deleted %T %s", obj, obj.GetName())
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	defer func() { x.r.Client = base }()
	return x.step()
}

// settle alternates operator steps and simulated workload syncs until the
// fleet reports Provisioned, never letting the operator delete a Pod or claim.
func (x *operationFixture) settle() {
	x.t.Helper()
	var f *fleet.CelldFleet
	for range 40 {
		f = x.operatorStep()
		x.syncWorkload()
		if readyReason(f) == "Provisioned" {
			return
		}
	}
	x.t.Fatalf("fleet never settled: %+v", meta.FindStatusCondition(f.Status.Conditions, "Ready"))
}

func (x *operationFixture) budget() int {
	x.t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), pdb); err != nil {
		x.t.Fatal(err)
	}
	return pdb.Spec.MaxUnavailable.IntValue()
}

// storesNothing requires that the operator persisted no state for the fleet.
func (x *operationFixture) storesNothing() {
	x.t.Helper()
	if raw := envReservation(x.t, x.r, x.f).Annotations[stateKey]; raw != "" {
		x.t.Fatalf("operator persisted state: %s", raw)
	}
}

func readyReason(f *fleet.CelldFleet) string {
	if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c != nil {
		return c.Reason
	}
	return ""
}

func TestPersistentRendersRollingStatefulSet(t *testing.T) {
	x := persistentFleet(t)
	sts := x.workload().(*appsv1.StatefulSet)
	pod := sts.Spec.Template.Spec
	if len(pod.InitContainers) != 0 || pod.Containers[0].Command != nil || len(pod.SchedulingGates) != 0 || pod.Containers[0].LivenessProbe != nil {
		t.Fatal("persistent pod still runs under the launcher")
	}
	if mode, _ := envValue(pod.Containers[0].Env, "CELLD_DURABILITY"); mode != "fleet" {
		t.Fatal("persistent fleet must use fleet durability")
	}
	if u := sts.Spec.UpdateStrategy; u.Type != appsv1.RollingUpdateStatefulSetStrategyType || u.RollingUpdate == nil || *u.RollingUpdate.Partition != 0 {
		t.Fatalf("the StatefulSet controller must roll every member: %+v", u)
	}
	if p := sts.Spec.PersistentVolumeClaimRetentionPolicy; p.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType || p.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("members must keep their claims: %+v", p)
	}
	if x.budget() != 1 {
		t.Fatal("the budget allows one voluntary disruption")
	}
	x.storesNothing()
}

func TestPersistentRestartAndUpgradeRollOnRetainedDisks(t *testing.T) {
	for _, change := range []string{"restart", "upgrade"} {
		t.Run(change, func(t *testing.T) {
			x := persistentFleet(t)
			claims := x.claimUIDs()
			pods := x.podUIDs()
			x.edit(func(f *fleet.CelldFleet) {
				if change == "restart" {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
				} else {
					f.Spec.RuntimeImage = fixtureRuntimeUpgrade
				}
			})
			// The operator only writes the template; with the workload
			// controller held, no member restarts.
			x.hold = true
			for range 3 {
				x.operatorStep()
				x.syncWorkload()
			}
			if after := x.podUIDs(); len(after) != 3 || after["alpha-0"] != pods["alpha-0"] || after["alpha-1"] != pods["alpha-1"] || after["alpha-2"] != pods["alpha-2"] {
				t.Fatal("the operator restarted a member itself")
			}
			sts := x.workload().(*appsv1.StatefulSet)
			if change == "restart" && sts.Spec.Template.Annotations[restartTokenAnnotation] != "r1" {
				t.Fatal("restart token not written to the template")
			}
			if change == "upgrade" && sts.Spec.Template.Spec.Containers[0].Image != fixtureRuntimeUpgrade {
				t.Fatal("runtime image not written to the template")
			}
			if reason := readyReason(x.operatorStep()); reason != "Provisioning" {
				t.Fatalf("pending rollout reported %s", reason)
			}
			x.hold = false
			x.settle()
			for name, uid := range x.podUIDs() {
				if uid == pods[name] {
					t.Fatalf("%s was not restarted", name)
				}
			}
			for name, uid := range x.claimUIDs() {
				if claims[name] != uid {
					t.Fatalf("rollout replaced disk %s", name)
				}
			}
			x.storesNothing()
		})
	}
}

func TestPersistentScaleInKeepsDisksAndGrowthReattaches(t *testing.T) {
	x := persistentFleet(t)
	claims := x.claimUIDs()
	x.desired(1)
	// One member per step: the next removal waits for the previous one.
	x.operatorStep()
	if replicas(x.workload()) != 2 {
		t.Fatal("contraction did not remove exactly one member")
	}
	x.operatorStep()
	if replicas(x.workload()) != 2 {
		t.Fatal("second removal started before the first rolled out")
	}
	x.settle()
	if replicas(x.workload()) != 1 {
		t.Fatal("contraction did not reach one member")
	}
	if got := x.claimUIDs(); len(got) != 3 || got["data-alpha-1"] != claims["data-alpha-1"] || got["data-alpha-2"] != claims["data-alpha-2"] {
		t.Fatalf("scale-in deleted a removed member's disk: %v", got)
	}
	x.desired(3)
	x.settle()
	if replicas(x.workload()) != 3 {
		t.Fatal("growth did not apply")
	}
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("growth did not reattach retained disk %s", name)
		}
	}
	x.storesNothing()
}

func TestPersistentContractionWaitsForRollout(t *testing.T) {
	x := persistentFleet(t)
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
		f.Spec.Replicas = 2
	})
	x.hold = true
	f := x.operatorStep()
	x.syncWorkload()
	sts := x.workload().(*appsv1.StatefulSet)
	if sts.Spec.Template.Annotations[restartTokenAnnotation] != "r1" {
		t.Fatal("a pending contraction held back the template")
	}
	if replicas(sts) != 3 || readyReason(f) != "Provisioning" {
		t.Fatalf("a removal overlapped a rollout: replicas %d, %s", replicas(sts), readyReason(f))
	}
	x.operatorStep()
	if replicas(x.workload()) != 3 {
		t.Fatal("a removal started while the rollout was held")
	}
	x.hold = false
	x.settle()
	if replicas(x.workload()) != 2 {
		t.Fatal("contraction did not resume after the rollout")
	}
}

func TestPersistentRefusesForeignClaims(t *testing.T) {
	x := persistentFleet(t)
	foreign := &corev1.PersistentVolumeClaim{Name: "data-alpha-3", Namespace: x.f.Namespace, Labels: map[string]string{FleetLabel: "another-fleet"}}
	if err := x.r.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	x.desired(4)
	if reason := readyReason(x.operatorStep()); reason != "StorageIdentityConflict" {
		t.Fatalf("growth onto a foreign claim reported %s", reason)
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("the StatefulSet may adopt another fleet's disk")
	}
	// A missing workload is not recreated onto it either.
	if err := x.r.Delete(t.Context(), x.workload()); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(x.operatorStep()); reason != "StorageIdentityConflict" {
		t.Fatalf("recreation onto a foreign claim reported %s", reason)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("workload recreated onto a foreign claim")
	}
}

func TestPersistentMissingWorkloadIsRecreatedOnItsDisks(t *testing.T) {
	x := persistentFleet(t)
	claims := x.claimUIDs()
	if err := x.r.Delete(t.Context(), x.workload()); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(x.operatorStep()); reason != "Provisioning" {
		t.Fatalf("missing workload reported %s", reason)
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("workload not recreated")
	}
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("recreation replaced disk %s", name)
		}
	}
}

func TestPersistentDeletionRemovesComputeThenEveryDisk(t *testing.T) {
	x := persistentFleet(t)
	// A member removed by scale-in keeps its disk until the fleet is deleted.
	x.desired(2)
	x.settle()
	if len(x.claimUIDs()) != 3 {
		t.Fatal("scale-in deleted a disk")
	}
	if err := x.r.Delete(t.Context(), x.f); err != nil {
		t.Fatal(err)
	}
	x.step()
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("workload not deleted first")
	}
	if len(x.claimUIDs()) != 3 {
		t.Fatal("disks deleted before compute")
	}
	x.step()
	if len(x.claimUIDs()) != 0 {
		t.Fatal("disks not deleted after compute")
	}
	if _, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)}); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
		t.Fatal("finalizer not released")
	}
	if err := x.r.Get(t.Context(), types.NamespacedName{Name: reservationName(x.f)}, &fleet.CelldStorageReservation{}); err != nil {
		t.Fatal("bucket reservation must stay permanent", err)
	}
}

// A fleet created by v0.0.5: an OnDelete StatefulSet, a member still held by
// the launcher gate, and strict bookkeeping on the reservation, including the
// identities of its claims.
func TestPersistentAdoptsStrictFleet(t *testing.T) {
	x := persistentFleet(t)
	claims := x.claimUIDs()
	sts := x.workload().(*appsv1.StatefulSet)
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
	if err := x.r.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	p := x.pod("alpha-2")
	if err := x.r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	p.ResourceVersion, p.UID = "", "old-alpha-2"
	p.Labels[revisionLabel] = "old"
	p.Spec.NodeName = ""
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: "celld.eric.dev/exclusive-volume"}}
	if err := x.r.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	res := envReservation(t, x.r, x.f)
	res.Annotations = map[string]string{}
	res.Annotations[stateKey] = `{"Version":1,"FleetUID":"` + string(x.f.UID) + `","WorkloadUID":"` + string(sts.UID) + `","Initial":3,"Applied":3,"RuntimeImage":"` + fixtureRuntime + `","Claims":{"data-alpha-0":"` + string(claims["data-alpha-0"]) + `"},"Operation":{"ID":"old-op","Kind":"Restart","Phase":"Requesting","Targets":[{"Pod":"alpha-2"}]}}`
	if err := x.r.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	x.settle()
	sts = x.workload().(*appsv1.StatefulSet)
	if sts.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		t.Fatal("adopted StatefulSet still waits for someone to delete its Pods")
	}
	for _, pod := range []string{"alpha-0", "alpha-1", "alpha-2"} {
		if got := x.pod(pod); got.Labels[revisionLabel] != sts.Status.UpdateRevision || len(got.Spec.SchedulingGates) != 0 {
			t.Fatalf("%s not rolled onto the current template", pod)
		}
	}
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("adoption replaced disk %s", name)
		}
	}
	x.storesNothing()
}

// Self-healing: a member that cannot come back on its own is replaced without
// an administrator, and one that can is left alone.

// setMember gives a member Pod a creation time and a readiness transition.
func (x *operationFixture) setMember(name string, created time.Time, ready corev1.ConditionStatus, since time.Time, edits ...func(*corev1.Pod)) {
	x.t.Helper()
	p := x.pod(name)
	p.CreationTimestamp = metav1.NewTime(created)
	if err := x.r.Update(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
	p = x.pod(name)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(since)}}
	for _, edit := range edits {
		edit(p)
	}
	if err := x.r.Status().Update(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
}

func (x *operationFixture) claim(name string) *corev1.PersistentVolumeClaim {
	x.t.Helper()
	c := &corev1.PersistentVolumeClaim{}
	if err := x.r.Get(x.t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: name}, c); err != nil {
		x.t.Fatal(err)
	}
	return c
}

// agedFleet is a settled fleet whose members have long been ready on disks
// older than their Pods.
func agedFleet(t *testing.T) *operationFixture {
	t.Helper()
	x := persistentFleet(t)
	for _, name := range []string{"alpha-0", "alpha-1", "alpha-2"} {
		x.setMember(name, x.clock.Add(-2*time.Hour), corev1.ConditionTrue, x.clock.Add(-time.Hour))
		c := x.claim("data-" + name)
		c.CreationTimestamp = metav1.NewTime(x.clock.Add(-24 * time.Hour))
		if err := x.r.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

func TestPersistentReplacesMemberWithLostVolume(t *testing.T) {
	x := persistentFleet(t)
	claims := x.claimUIDs()
	c := x.claim("data-alpha-1")
	c.Status.Phase = corev1.ClaimLost
	if err := x.r.Status().Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(x.step()); reason != "LifecycleProgress" {
		t.Fatalf("lost volume reported %s", reason)
	}
	if _, kept := x.claimUIDs()["data-alpha-1"]; kept {
		t.Fatal("claim of a lost volume kept")
	}
	if _, kept := x.podUIDs()["alpha-1"]; kept {
		t.Fatal("member on a lost volume not replaced")
	}
	x.converge()
	for name, uid := range x.claimUIDs() {
		if fresh := uid != claims[name]; fresh != (name == "data-alpha-1") {
			t.Fatalf("disk %s replaced=%v", name, fresh)
		}
	}
}

func TestStatefulSetMembersLeftOnUnreachableNodesAreForceDeleted(t *testing.T) {
	for _, profile := range []string{"PersistentFleet", "Bucket"} {
		t.Run(profile, func(t *testing.T) { forceDeletesPodLeftOnUnreachableNode(t, profile) })
	}
}

func forceDeletesPodLeftOnUnreachableNode(t *testing.T, profile string) {
	x := newOperationFixture(t, profile)
	x.converge()
	claims := x.claimUIDs()
	p := x.pod("alpha-2")
	p.Finalizers = []string{"test.celld.eric.dev/node-unreachable"}
	if err := x.r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	forced := 0
	base := x.r.Client
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if o := (&client.DeleteOptions{}).ApplyOptions(opts); obj.GetName() == "alpha-2" && o.GracePeriodSeconds != nil && *o.GracePeriodSeconds == 0 {
				forced++
			}
			if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
				t.Fatal("force-deleting a Pod released its disk")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	defer func() { x.r.Client = base }()
	x.clock = time.Now().Add(time.Minute)
	x.step()
	if forced != 0 {
		t.Fatal("force-deleted a Pod still within its termination margin")
	}
	x.clock = time.Now().Add(terminationMargin + time.Minute)
	if reason := readyReason(x.step()); reason != "LifecycleProgress" || forced != 1 {
		t.Fatalf("Pod left on an unreachable node not force-deleted: %s, %d", reason, forced)
	}
	if len(x.claimUIDs()) != len(claims) {
		t.Fatal("disk changed")
	}
}

func TestPersistentReplacesMemberThatCannotReturn(t *testing.T) {
	x := agedFleet(t)
	claims := x.claimUIDs()
	x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Minute))
	f := x.step()
	if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c == nil || c.Reason != "Provisioning" || !strings.Contains(c.Message, "alpha-1 is down; it is replaced on a fresh disk at") {
		t.Fatalf("pending replacement not reported: %+v", c)
	}
	if x.claimUIDs()["data-alpha-1"] != claims["data-alpha-1"] {
		t.Fatal("replaced a member before the delay")
	}
	x.clock = x.clock.Add(DefaultMemberReplacementDelay)
	if reason := readyReason(x.step()); reason != "LifecycleProgress" {
		t.Fatalf("member that cannot return reported %s", reason)
	}
	if _, kept := x.claimUIDs()["data-alpha-1"]; kept {
		t.Fatal("disk of a member that cannot return kept")
	}
	x.converge()
	for name, uid := range x.claimUIDs() {
		if fresh := uid != claims[name]; fresh != (name == "data-alpha-1") {
			t.Fatalf("disk %s replaced=%v", name, fresh)
		}
	}
}

func TestPersistentLeavesMembersThatAFreshDiskWouldNotHelp(t *testing.T) {
	for name, setup := range map[string]func(x *operationFixture){
		"another member recently restarted": func(x *operationFixture) {
			x.setMember("alpha-0", x.clock.Add(-2*time.Minute), corev1.ConditionTrue, x.clock.Add(-time.Minute))
		},
		"two members down": func(x *operationFixture) {
			x.setMember("alpha-2", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
		},
		"waiting on its image": func(x *operationFixture) {
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour), func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "celld", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
			})
		},
		"already on a disk made for its Pod": func(x *operationFixture) {
			c := x.claim("data-alpha-1")
			c.CreationTimestamp = metav1.NewTime(x.clock.Add(-2 * time.Hour))
			if err := x.r.Update(x.t.Context(), c); err != nil {
				x.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := agedFleet(t)
			claims := x.claimUIDs()
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
			setup(x)
			x.clock = x.clock.Add(time.Minute)
			x.operatorStep()
			if len(x.claimUIDs()) != 3 || x.claimUIDs()["data-alpha-1"] != claims["data-alpha-1"] {
				t.Fatal("replaced a member that a fresh disk would not help")
			}
		})
	}
}

// spreadConflict is the scheduler's verdict on a member whose disk pins it to
// a zone that the zone spread no longer allows (#79). The fleet's status
// quotes it without the final period.
const spreadConflict = "0/3 nodes are available: 1 node(s) didn't match pod topology spread constraints, 2 node(s) didn't match PersistentVolume's node affinity. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."

var spreadReason = strings.TrimSuffix(spreadConflict, ".")

// pendMember recreates a member's Pod on the same claim, as the StatefulSet
// does, and leaves it off every node. A non-empty verdict is the scheduler's
// message after it found no node for the Pod.
func (x *operationFixture) pendMember(name string, created time.Time, verdict string) {
	x.t.Helper()
	p := x.pod(name)
	if err := x.r.Delete(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
	p.ResourceVersion, p.UID = "", types.UID(name+"-pending")
	p.CreationTimestamp = metav1.NewTime(created)
	p.Spec.NodeName = ""
	p.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if verdict != "" {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: verdict, LastTransitionTime: metav1.NewTime(created)}}
	}
	if err := x.r.Create(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
}

// #79: an operator upgrade found alpha-1 down long past the replacement delay,
// as v0.0.5 leaves a member whose disk its launcher retired. Replacing it
// while the rollout restarts alpha-2 leaves both Pods pending at once, and the
// fresh disk can take the zone alpha-2's disk pins it to. Nothing is replaced
// until the StatefulSet has caught up, and the rollout then restarts alpha-1
// on its own disk.
func TestPersistentFreshDiskWaitsForTheStatefulSetToCatchUp(t *testing.T) {
	x := agedFleet(t)
	claims := x.claimUIDs()
	x.setMember("alpha-1", x.clock.Add(-3*time.Hour), corev1.ConditionFalse, x.clock.Add(-2*time.Hour))
	x.edit(func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = fixtureRuntimeUpgrade })
	// The reconcile that writes the new template replaces nothing.
	if reason := readyReason(x.operatorStep()); reason != "Provisioning" {
		t.Fatalf("upgrade reported %s", reason)
	}
	// Neither does one before the StatefulSet has observed that template.
	sts := x.workload().(*appsv1.StatefulSet)
	sts.Generation = sts.Status.ObservedGeneration + 1
	if err := x.r.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	x.operatorStep()
	x.settle()
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("the upgrade replaced disk %s", name)
		}
	}
}

// A lost volume is replaced once the scheduler has decided every other
// member's Pod, here alpha-1's, which is being recreated. Either decision
// counts: a Pod on a node holds its zone, and one the scheduler cannot place
// must not hold up the replacement.
func TestPersistentFreshDiskWaitsForTheSchedulerToDecideEveryMember(t *testing.T) {
	for name, decide := range map[string]func(x *operationFixture){
		"placed": func(x *operationFixture) {
			p := x.pod("alpha-1")
			p.Spec.NodeName = "host-1"
			if err := x.r.Update(x.t.Context(), p); err != nil {
				x.t.Fatal(err)
			}
		},
		"unschedulable": func(x *operationFixture) {
			p := x.pod("alpha-1")
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: spreadConflict, LastTransitionTime: metav1.NewTime(x.clock)}}
			if err := x.r.Status().Update(x.t.Context(), p); err != nil {
				x.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := persistentFleet(t)
			claims := x.claimUIDs()
			c := x.claim("data-alpha-2")
			c.Status.Phase = corev1.ClaimLost
			if err := x.r.Status().Update(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			x.pendMember("alpha-1", x.clock, "")
			x.operatorStep()
			decide(x)
			if reason := readyReason(x.step()); reason != "LifecycleProgress" {
				t.Fatalf("lost volume reported %s", reason)
			}
			if _, kept := x.claimUIDs()["data-alpha-2"]; kept {
				t.Fatal("claim of a lost volume kept")
			}
			if x.claimUIDs()["data-alpha-1"] != claims["data-alpha-1"] {
				t.Fatal("replaced the member being scheduled")
			}
		})
	}
}

// A member whose disk pins it to a zone that another member has taken cannot
// be scheduled. The status names it with the scheduler's reason, and after the
// replacement delay it is replaced like any member that cannot come back.
// With every other member on a node, the zone spread places its fresh disk in
// the zone the fleet is missing.
func TestPersistentReplacesAMemberItsDiskKeepsOffEveryNode(t *testing.T) {
	x := agedFleet(t)
	claims := x.claimUIDs()
	x.pendMember("alpha-2", x.clock.Add(-time.Minute), spreadConflict)
	f := x.operatorStep()
	c := meta.FindStatusCondition(f.Status.Conditions, "Ready")
	if c == nil || c.Reason != "Provisioning" || !strings.Contains(c.Message, "member alpha-2 cannot be scheduled ("+spreadReason+"); it is replaced on a fresh disk at") {
		t.Fatalf("unschedulable member not reported: %+v", c)
	}
	if meta.IsStatusConditionTrue(f.Status.Conditions, "Blocked") {
		t.Fatal("a member the operator will replace blocks the fleet")
	}
	x.clock = x.clock.Add(DefaultMemberReplacementDelay)
	if reason := readyReason(x.step()); reason != "LifecycleProgress" {
		t.Fatalf("unschedulable member reported %s", reason)
	}
	x.converge()
	for name, uid := range x.claimUIDs() {
		if fresh := uid != claims[name]; fresh != (name == "data-alpha-2") {
			t.Fatalf("disk %s replaced=%v", name, fresh)
		}
	}
}

// A member the scheduler cannot place, and that the operator does not
// replace, blocks the fleet once it has waited out the replacement delay.
func TestPersistentBlocksOnAMemberItCannotPlaceOrReplace(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(x *operationFixture)
		why   string
	}{
		// The deadlock #79 reported: alpha-1 came back on a fresh disk in the
		// zone that alpha-2's disk needs, and its recovery waits for alpha-2.
		// Neither is replaced while the other is down.
		"another member down": {
			setup: func(x *operationFixture) {
				x.setMember("alpha-1", x.clock.Add(-2*time.Minute), corev1.ConditionFalse, x.clock.Add(-time.Minute))
				c := x.claim("data-alpha-1")
				c.CreationTimestamp = metav1.NewTime(x.clock.Add(-2 * time.Minute))
				if err := x.r.Update(x.t.Context(), c); err != nil {
					x.t.Fatal(err)
				}
			},
			why: "; it is not replaced while member alpha-1 is also down",
		},
		// A member with no disk yet waits for a node; a fresh disk would not
		// help it.
		"no disk yet": {
			setup: func(x *operationFixture) {
				c := x.claim("data-alpha-2")
				c.Status.Phase = corev1.ClaimPending
				if err := x.r.Status().Update(x.t.Context(), c); err != nil {
					x.t.Fatal(err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := agedFleet(t)
			claims := x.claimUIDs()
			x.pendMember("alpha-2", x.clock.Add(-time.Minute), spreadConflict)
			tc.setup(x)
			f := x.operatorStep()
			if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c == nil || c.Reason != "Provisioning" || !strings.HasSuffix(c.Message, "; member alpha-2 cannot be scheduled ("+spreadReason+")") {
				t.Fatalf("unschedulable member not reported: %+v", c)
			}
			if meta.IsStatusConditionTrue(f.Status.Conditions, "Blocked") {
				t.Fatal("blocked before the replacement delay")
			}
			since := x.clock.Add(-time.Minute).UTC().Format(time.RFC3339)
			x.clock = x.clock.Add(DefaultMemberReplacementDelay)
			f = x.operatorStep()
			c := meta.FindStatusCondition(f.Status.Conditions, "Blocked")
			want := "Member alpha-2 has not been scheduled since " + since + ": " + spreadReason + tc.why
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "MemberUnschedulable" || c.Message != want {
				t.Fatalf("fleet not blocked on the member: %+v", c)
			}
			if !meta.IsStatusConditionTrue(f.Status.Conditions, "InfrastructureReady") || meta.IsStatusConditionTrue(f.Status.Conditions, "Progressing") {
				t.Fatalf("blocked on a member with its objects in place: %+v", f.Status.Conditions)
			}
			if len(x.claimUIDs()) != len(claims) {
				t.Fatal("replaced a disk")
			}
		})
	}
}

// Once the other member is back and the rest of the fleet has been ready for
// five minutes, the member the fleet was blocked on is replaced.
func TestPersistentReplacesTheMemberItWasBlockedOnOnceTheOtherReturns(t *testing.T) {
	x := agedFleet(t)
	x.setMember("alpha-1", x.clock.Add(-2*time.Minute), corev1.ConditionFalse, x.clock.Add(-time.Minute))
	x.pendMember("alpha-2", x.clock.Add(-time.Minute), spreadConflict)
	x.clock = x.clock.Add(DefaultMemberReplacementDelay)
	if reason := readyReason(x.operatorStep()); reason != "MemberUnschedulable" {
		t.Fatalf("stuck members reported %s", reason)
	}
	x.setMember("alpha-1", x.clock.Add(-12*time.Minute), corev1.ConditionTrue, x.clock)
	if reason := readyReason(x.operatorStep()); reason != "MemberUnschedulable" {
		t.Fatalf("replaced a member before the rest of the fleet had been ready for five minutes: %s", reason)
	}
	x.clock = x.clock.Add(absorbWindow)
	if reason := readyReason(x.step()); reason != "LifecycleProgress" {
		t.Fatalf("member blocked on its disk reported %s", reason)
	}
	if _, kept := x.podUIDs()["alpha-2"]; kept {
		t.Fatal("member blocked on its disk not replaced")
	}
}

// The operator restarted after deleting a replaced member's claim but before
// deleting its Pod: the Pod still holds the claim, so it is deleted next.
func TestPersistentFinishesInterruptedReplacement(t *testing.T) {
	x := persistentFleet(t)
	c := x.claim("data-alpha-1")
	c.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if err := x.r.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	x.step()
	if _, kept := x.podUIDs()["alpha-1"]; kept {
		t.Fatal("Pod holding a replaced member's claim not deleted")
	}
	if _, kept := x.podUIDs()["alpha-0"]; !kept {
		t.Fatal("deleted another member")
	}
}

// A Pod that carries the fleet's label but is not the StatefulSet's, such as
// a debugging or probe Pod, is not a member: it neither blocks reconciliation
// nor is ever deleted.
func TestPersistentIgnoresPodsTheStatefulSetDoesNotControl(t *testing.T) {
	x := persistentFleet(t)
	stray := &corev1.Pod{Name: "debug", Namespace: x.f.Namespace, Labels: labels(x.f), Finalizers: []string{"test.celld.eric.dev/hold"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "debug", Image: "busybox"}}}}
	if err := x.r.Create(t.Context(), stray); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), stray); err != nil {
		t.Fatal(err)
	}
	x.clock = time.Now().Add(terminationMargin + time.Hour)
	if reason := readyReason(x.operatorStep()); reason != "Provisioned" {
		t.Fatalf("a stray labeled Pod changed the fleet's status: %s", reason)
	}
}

// Growth in runs: kept disks pin their members to zones, and a fresh disk
// created alongside them can take a zone one of them needs (#79).

// scheduleNoNode records the scheduler's verdict that it found no node for a
// member's pending Pod.
func (x *operationFixture) scheduleNoNode(name string) {
	x.t.Helper()
	p := x.pod(name)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: spreadConflict, LastTransitionTime: metav1.NewTime(x.clock)}}
	if err := x.r.Status().Update(x.t.Context(), p); err != nil {
		x.t.Fatal(err)
	}
}

func waitingFor(t *testing.T, f *fleet.CelldFleet, want string) {
	t.Helper()
	if c := meta.FindStatusCondition(f.Status.Conditions, "Ready"); c == nil || c.Reason != "Provisioning" || c.Message != want {
		t.Fatalf("growth wait not reported: %+v", c)
	}
}

// Growth first reattaches the run of ordinals that kept their disks. Members
// on fresh disks are added only once the scheduler has placed each existing
// member, or found no node for it, so every zone a kept disk holds is counted.
func TestPersistentGrowthReattachesKeptDisksBeforeAddingFreshOnes(t *testing.T) {
	x := persistentFleet(t)
	x.desired(1)
	x.settle()
	claims := x.claimUIDs()
	x.desired(5)
	x.operatorStep()
	if n := replicas(x.workload()); n != 3 {
		t.Fatalf("growth wrote %d replicas, want the 3 with kept disks", n)
	}
	x.syncWorkload()
	x.pendMember("alpha-1", x.clock, "")
	x.pendMember("alpha-2", x.clock, "")
	waitingFor(t, x.operatorStep(), "Waiting for the scheduler to place member alpha-1, or find no node for it, before adding members alpha-3 to alpha-4 on fresh disks")
	p := x.pod("alpha-1")
	p.Spec.NodeName = "host-1"
	if err := x.r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	waitingFor(t, x.operatorStep(), "Waiting for the scheduler to place member alpha-2, or find no node for it, before adding members alpha-3 to alpha-4 on fresh disks")
	if n := replicas(x.workload()); n != 3 {
		t.Fatalf("added fresh disks while a kept disk's member was undecided: %d replicas", n)
	}
	// A member the scheduler cannot place does not hold growth back.
	x.scheduleNoNode("alpha-2")
	x.operatorStep()
	if n := replicas(x.workload()); n != 5 {
		t.Fatalf("fresh members not added once every member was decided: %d replicas", n)
	}
	x.syncWorkload()
	for name, uid := range x.claimUIDs() {
		if old, ok := claims[name]; ok != (uid == old) {
			t.Fatalf("disk %s kept=%v", name, ok)
		}
	}
	x.storesNothing()
}

// A run of fresh disks ends at the next ordinal that kept its disk, here
// alpha-2 after alpha-1's claim was deleted by hand.
func TestPersistentGrowthReattachesAfterAFreshRun(t *testing.T) {
	x := persistentFleet(t)
	x.desired(1)
	x.settle()
	if err := x.r.Delete(t.Context(), x.claim("data-alpha-1")); err != nil {
		t.Fatal(err)
	}
	claims := x.claimUIDs()
	x.desired(3)
	x.operatorStep()
	if n := replicas(x.workload()); n != 2 {
		t.Fatalf("growth wrote %d replicas, want the fresh run to end before alpha-2", n)
	}
	waitingFor(t, x.operatorStep(), "Waiting for the scheduler to place member alpha-1, or find no node for it, before adding member alpha-2 on its kept disk")
	x.syncWorkload()
	x.settle()
	if n := replicas(x.workload()); n != 3 {
		t.Fatalf("growth stopped at %d replicas", n)
	}
	got := x.claimUIDs()
	if got["data-alpha-2"] != claims["data-alpha-2"] || got["data-alpha-1"] == "" {
		t.Fatalf("growth did not reattach alpha-2's disk after alpha-1's fresh one: %v", got)
	}
}

// A claim from another fleet anywhere in the requested growth blocks all of
// it, including the run of kept disks before it.
func TestPersistentForeignClaimBlocksEveryGrowthStep(t *testing.T) {
	x := persistentFleet(t)
	x.desired(1)
	x.settle()
	foreign := &corev1.PersistentVolumeClaim{Name: "data-alpha-4", Namespace: x.f.Namespace, Labels: map[string]string{FleetLabel: "another-fleet"}}
	if err := x.r.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	x.desired(5)
	if reason := readyReason(x.operatorStep()); reason != "StorageIdentityConflict" {
		t.Fatalf("growth onto a foreign claim reported %s", reason)
	}
	if n := replicas(x.workload()); n != 1 {
		t.Fatalf("growth toward a foreign claim wrote %d replicas", n)
	}
}

// A policy step that spans kept and fresh disks is cut at the end of the kept
// run and recorded as the step taken: its redistribution is judged against
// the one member it added, and the rest waits for the policy to ask again.
func TestPersistentPolicyGrowthStopsAtTheKeptRun(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.f = enableCapacity(t, x.r, x.f, "Automatic")
	x.desired(2)
	x.settle()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Capacity.ScaleOutStep = 2 })
	x.cpu = 1000
	added := x.awaitReplicas(3)
	s := x.state().Capacity
	if !s.LastAction.Equal(added) || s.Addition == nil || s.Addition.Target != 3 {
		t.Fatalf("the cut step was recorded as %v with %+v", s.LastAction, s.Addition)
	}
	for range 4 {
		x.tick()
	}
	if n := replicas(x.workload()); n != 3 {
		t.Fatalf("the rest of the cut step was added without the policy: %d replicas", n)
	}
}

// A full stop returns at the declared count even when growth from zero takes
// several runs, here because alpha-1's claim was deleted while stopped.
func TestPersistentFullStopReturnsInRuns(t *testing.T) {
	for _, mode := range []string{"", "Automatic"} {
		t.Run("mode="+mode, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			if mode != "" {
				x.f = enableCapacity(t, x.r, x.f, mode)
			}
			x.settle()
			x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
			x.step()
			sts := x.workload().(*appsv1.StatefulSet)
			sts.Spec.Replicas = new(int32(0))
			if err := x.r.Update(t.Context(), sts); err != nil {
				t.Fatal(err)
			}
			x.syncWorkload()
			if err := x.r.Delete(t.Context(), x.claim("data-alpha-1")); err != nil {
				t.Fatal(err)
			}
			claims := x.claimUIDs()
			x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: false} })
			x.operatorStep()
			if n := replicas(x.workload()); n != 1 {
				t.Fatalf("the restart wrote %d replicas, want alpha-0's kept run", n)
			}
			x.syncWorkload()
			x.settle()
			if n := replicas(x.workload()); n != 3 {
				t.Fatalf("fleet returned at %d members, want 3", n)
			}
			got := x.claimUIDs()
			if got["data-alpha-0"] != claims["data-alpha-0"] || got["data-alpha-2"] != claims["data-alpha-2"] {
				t.Fatalf("the restart replaced a kept disk: %v", got)
			}
			if s := x.state(); s != nil && s.Capacity != nil && s.Capacity.Addition != nil {
				t.Fatalf("a full stop was judged as a policy addition: %+v", s.Capacity.Addition)
			}
		})
	}
}
