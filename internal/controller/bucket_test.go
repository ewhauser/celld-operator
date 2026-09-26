package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const fixtureRuntimeUpgrade = "ghcr.io/ewhauser/celld@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

var errInvalidFixture = errors.New("invalid state fixture")

// Bucket fleets are plain workloads (ADR 0023): no launcher, no strict proof,
// no current operation. The workload controller rolls one member at a time.

func bucketFleet(t *testing.T, layout string) *operationFixture {
	t.Helper()
	if layout == "Ordered" {
		return newOperationFixture(t, "Bucket")
	}
	return newOperationFixture(t, "Deployment")
}

func TestBucketRendersPlainWorkload(t *testing.T) {
	for _, layout := range []string{"Deployment", "Ordered"} {
		t.Run(layout, func(t *testing.T) {
			x := bucketFleet(t, layout)
			var pod corev1.PodSpec
			switch w := x.workload().(type) {
			case *appsv1.Deployment:
				pod = w.Spec.Template.Spec
				if w.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || w.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 1 || w.Spec.Strategy.RollingUpdate.MaxSurge.IntValue() != 0 {
					t.Fatalf("deployment must roll one member at a time: %+v", w.Spec.Strategy)
				}
			case *appsv1.StatefulSet:
				pod = w.Spec.Template.Spec
				if w.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
					t.Fatalf("ordered layout must roll: %+v", w.Spec.UpdateStrategy)
				}
			}
			c := pod.Containers[0]
			if len(pod.InitContainers) != 0 || c.Command != nil || c.LivenessProbe != nil || len(c.Ports) != 2 {
				t.Fatal("bucket pod still runs under the launcher")
			}
			for _, v := range pod.Volumes {
				if v.Name == "launcher" || v.Name == "launcher-key" {
					t.Fatal("bucket pod mounts launcher material")
				}
			}
			for _, g := range pod.SchedulingGates {
				if g.Name == "celld.eric.dev/exclusive-volume" {
					t.Fatal("bucket pod carries the launcher gate")
				}
			}
			if mode, _ := envValue(c.Env, "CELLD_DURABILITY"); mode != "bucket" {
				t.Fatal("bucket fleet must acknowledge only on bucket proof")
			}
			pdb := &policyv1.PodDisruptionBudget{}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), pdb); err != nil {
				t.Fatal(err)
			}
			if pdb.Spec.MaxUnavailable.IntValue() != 1 {
				t.Fatal("bucket budget must allow one voluntary disruption")
			}
			policy := &networkingv1.NetworkPolicy{}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), policy); err != nil {
				t.Fatal(err)
			}
			for _, rule := range policy.Spec.Ingress {
				for _, p := range rule.Ports {
					if p.Port.IntValue() == 8083 {
						t.Fatal("bucket policy admits the launcher port")
					}
				}
			}
			if s := x.state(); s != nil {
				t.Fatalf("bucket fleet persisted state %+v", s)
			}
			reason(t, reconcile(t, x.r, x.f), "Provisioned")
		})
	}
}

func TestBucketScalesOneMemberAtATime(t *testing.T) {
	for _, layout := range []string{"Deployment", "Ordered"} {
		t.Run(layout, func(t *testing.T) {
			x := bucketFleet(t, layout)
			x.edit(func(f *fleet.CelldFleet) {
				f.Spec.Placement.AZCount = 1
				f.Spec.Placement.Zones = []string{"us-east-1a"}
			})
			x.desired(1)
			x.step()
			if replicas(x.workload()) != 2 {
				t.Fatalf("first step removes one member, got %d", replicas(x.workload()))
			}
			x.step()
			if replicas(x.workload()) != 2 {
				t.Fatal("next member removed before the previous change rolled out")
			}
			x.syncWorkload()
			x.step()
			if replicas(x.workload()) != 1 {
				t.Fatal("contraction incomplete")
			}
			x.syncWorkload()
			x.desired(4)
			x.step()
			if replicas(x.workload()) != 4 {
				t.Fatal("growth is applied directly")
			}
		})
	}
}

func TestBucketRestartAndUpgradeRollWithoutDowntimePermission(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
	})
	x.step()
	d := x.workload().(*appsv1.Deployment)
	if d.Spec.Template.Annotations[restartTokenAnnotation] != "r1" || replicas(d) != 3 {
		t.Fatal("restart token did not roll the template")
	}
	x.edit(func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = fixtureRuntimeUpgrade })
	x.step()
	d = x.workload().(*appsv1.Deployment)
	if d.Spec.Template.Spec.Containers[0].Image != fixtureRuntimeUpgrade || replicas(d) != 3 {
		t.Fatal("upgrade did not roll the template")
	}
	if s := x.state(); s != nil {
		t.Fatalf("bucket maintenance persisted state %+v", s)
	}
}

func TestRolledOutRequiresObservedUpdatedReadyReplicas(t *testing.T) {
	d := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: new(int32(3))}}
	d.Generation = 2
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3}
	if !rolledOut(d) {
		t.Fatal("complete rollout not recognized")
	}
	for name, edit := range map[string]func(*appsv1.DeploymentStatus){
		"unobserved": func(s *appsv1.DeploymentStatus) { s.ObservedGeneration = 1 },
		"old member": func(s *appsv1.DeploymentStatus) { s.UpdatedReplicas = 2 },
		"unready":    func(s *appsv1.DeploymentStatus) { s.ReadyReplicas = 2 },
		"surplus":    func(s *appsv1.DeploymentStatus) { s.Replicas = 4 },
	} {
		c := d.DeepCopy()
		edit(&c.Status)
		if rolledOut(c) {
			t.Fatalf("%s rollout reported complete", name)
		}
	}
	sts := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Replicas: new(int32(2)), UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType}}}
	sts.Status = appsv1.StatefulSetStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, CurrentRevision: "a", UpdateRevision: "b"}
	if rolledOut(sts) {
		t.Fatal("rolling statefulset revision change reported complete")
	}
	// OnDelete never advances currentRevision; updated replicas decide.
	sts.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	if !rolledOut(sts) {
		t.Fatal("OnDelete rollout with every replica updated never completes")
	}
	sts.Status.UpdatedReplicas = 1
	if rolledOut(sts) {
		t.Fatal("OnDelete rollout with an outdated replica reported complete")
	}
}

func TestBucketDriftIsCorrected(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	d := x.workload().(*appsv1.Deployment)
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: "extra", Image: "unexpected"})
	d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "CELLD_BUCKET", Value: "s3://other-fleet"})
	if err := x.r.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Ingress = append(policy.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{})
	if err := x.r.Update(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	x.step()
	if !matches(workload(x.f, x.r.Options), x.workload()) {
		t.Fatal("workload drift not corrected")
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), policy); err != nil {
		t.Fatal(err)
	}
	if !matches(prerequisites(x.f, x.r.Options)[0], policy) {
		t.Fatal("isolation drift not corrected")
	}
}

func TestBucketRefusesForeignObjects(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	d := x.workload().(*appsv1.Deployment)
	d.Labels[FleetLabel] = "other-uid"
	if err := x.r.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x.r, x.f), "LifecycleBlocked")
	if x.workload().GetLabels()[FleetLabel] != "other-uid" {
		t.Fatal("foreign workload adopted")
	}
}

func TestBucketRecreatesMissingWorkload(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	if err := x.r.Delete(t.Context(), x.workload()); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x.r, x.f), "Provisioning")
	if replicas(x.workload()) != 3 {
		t.Fatal("missing bucket workload not recreated")
	}
}

func TestBucketMigratesStrictFleet(t *testing.T) {
	// A fleet created under 0022 has a launcher template, a Recreate strategy
	// and a zero-disruption budget. Convergence replaces all three in place.
	x := bucketFleet(t, "Deployment")
	legacy := x.f.DeepCopy()
	legacy.Spec.Profile = "PersistentFleet"
	strict := podTemplate(legacy, x.r.Options)
	strict.Spec.SchedulingGates = nil
	d := x.workload().(*appsv1.Deployment)
	d.Spec.Template.Spec.InitContainers = strict.Spec.InitContainers
	d.Spec.Template.Spec.Containers[0].Command = strict.Spec.Containers[0].Command
	d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	if err := x.r.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), pdb); err != nil {
		t.Fatal(err)
	}
	pdb.Spec.MaxUnavailable = new(intstr.FromInt32(0))
	if err := x.r.Update(t.Context(), pdb); err != nil {
		t.Fatal(err)
	}
	x.step()
	d = x.workload().(*appsv1.Deployment)
	if len(d.Spec.Template.Spec.InitContainers) != 0 || d.Spec.Template.Spec.Containers[0].Command != nil || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatal("launcher template not migrated")
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatal("budget not migrated")
	}
}

func TestBucketDropsStrictOperationState(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	recorder := events.NewFakeRecorder(16)
	x.r.Recorder = recorder
	res := envReservation(t, x.r, x.f)
	res.Annotations = map[string]string{stateKey: `{"Version":1,"FleetUID":"` + string(x.f.UID) + `","Applied":3,"Operation":{"ID":"op","Kind":"Restart","Phase":"Requesting"}}`}
	if err := x.r.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x.r, x.f), "Provisioned")
	reconcile(t, x.r, x.f)
	if s := x.state(); s != nil {
		t.Fatalf("strict operation state kept: %+v", s)
	}
	superseded := 0
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, "OperationSuperseded") {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("want one superseded event, got %d", superseded)
	}
	r := &Reconciler{}
	s := &fleetState{Version: 1, FleetUID: x.f.UID}
	if got := r.currentState(x.f, &loadedState{j: s, err: errInvalidFixture}); got.j != nil || got.err != nil {
		t.Fatal("unreadable state must not block a bucket fleet")
	}
	foreign := &fleetState{FleetUID: types.UID("other")}
	if got := r.currentState(x.f, &loadedState{j: foreign}); got.j != nil {
		t.Fatal("foreign state reused")
	}
}

func TestBucketInvalidStateDoesNotBlock(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	res := &fleet.CelldStorageReservation{}
	if err := x.r.Get(t.Context(), types.NamespacedName{Name: reservationName(x.f)}, res); err != nil {
		t.Fatal(err)
	}
	res.Annotations = map[string]string{}
	res.Annotations[stateKey] = `{"Version":99}`
	if err := x.r.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x.r, x.f), "Provisioned")
	if s := x.state(); s != nil {
		t.Fatalf("unreadable state kept: %+v", s)
	}
}

func TestBucketPauseSuspendsChanges(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Replicas = 4
		f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
	})
	reason(t, reconcile(t, x.r, x.f), "MaintenancePaused")
	if replicas(x.workload()) != 3 {
		t.Fatal("paused fleet changed")
	}
}

func TestBucketAutomaticContraction(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Placement.AZCount = 1
		f.Spec.Placement.Zones = []string{"us-east-1a"}
	})
	x.f = enableCapacity(t, x.r, x.f, "Automatic")
	for range 50 {
		x.step()
		if replicas(x.workload()) < 3 {
			break
		}
		x.clock = x.clock.Add(15 * time.Second)
	}
	if replicas(x.workload()) != 2 {
		t.Fatalf("low demand did not contract a Deployment by one member: %d", replicas(x.workload()))
	}
}

func TestBucketDeletionRemovesComputeOnly(t *testing.T) {
	x := bucketFleet(t, "Deployment")
	if err := x.r.Delete(t.Context(), x.f); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &appsv1.Deployment{}); err == nil {
		t.Fatal("workload not deleted")
	}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), &fleet.CelldFleet{}); err == nil {
		t.Fatal("finalizer not released")
	}
	if err := x.r.Get(t.Context(), types.NamespacedName{Name: reservationName(x.f)}, &fleet.CelldStorageReservation{}); err != nil {
		t.Fatal("bucket reservation must stay permanent", err)
	}
}

// A removal that waits for a rollout must not hold back the zone assignment
// the rollout's new Pods need, or neither could finish.
func TestOrderedBucketReleasesGatesWhileContractionWaits(t *testing.T) {
	x := bucketFleet(t, "Ordered")
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
		f.Spec.Replicas = 2
	})
	x.hold = true
	x.step()
	x.syncWorkload()
	// The StatefulSet controller replaces the highest member with a gated Pod.
	old := x.pod("alpha-2")
	if err := x.r.Delete(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	p := old.DeepCopy()
	p.ResourceVersion, p.UID = "", "alpha-2-rolled"
	p.Spec.NodeName = ""
	p.Spec.NodeSelector = nil
	p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: bucketZoneGate}}
	p.Status = corev1.PodStatus{}
	if err := x.r.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if got := readyReason(x.step()); got != "Provisioning" {
		t.Fatalf("removal did not wait for the rollout: %s", got)
	}
	if replicas(x.workload()) != 3 {
		t.Fatal("removal overlapped the rollout")
	}
	if gates := x.pod("alpha-2").Spec.SchedulingGates; len(gates) != 0 {
		t.Fatal("zone gate held while the removal waits")
	}
}
