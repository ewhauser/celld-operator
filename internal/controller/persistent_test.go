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
// applies. The StatefulSet controller restarts members; the operator never
// deletes a Pod or a claim while the fleet exists, and stores nothing.

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
