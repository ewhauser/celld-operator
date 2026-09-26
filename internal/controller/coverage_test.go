package controller

import (
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// Unit-level counterparts of the extended kind suite: larger fleets, admission
// mutations and a voluntary eviction during a rollout.

func fiveMembers(x *operationFixture) { x.f.Spec.Replicas = 5 }

// restartStep runs one settled rollout step and returns the member it
// replaced, or "" when none was.
func (x *operationFixture) restartStep() string {
	x.t.Helper()
	before := x.podUIDs()
	x.step()
	after := x.podUIDs()
	replaced := ""
	for name, uid := range before {
		if after[name] != uid {
			if replaced != "" {
				x.t.Fatalf("replaced %s and %s in one step", replaced, name)
			}
			replaced = name
		}
	}
	x.syncWorkload()
	return replaced
}

func TestFiveMemberFleetRollsAndContractsOneAtATime(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet", fiveMembers)
	x.converge()
	claims := x.claimUIDs()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"} })
	x.step()
	x.syncWorkload()
	var order []string
	for range 5 {
		order = append(order, x.restartStep())
	}
	want := []string{"alpha-4", "alpha-3", "alpha-2", "alpha-1", "alpha-0"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("five members restarted in order %v, want %v", order, want)
		}
	}
	for name, uid := range x.claimUIDs() {
		if claims[name] != uid {
			t.Fatalf("restart replaced disk %s", name)
		}
	}
	x.converge()
	x.desired(3)
	for _, removed := range []string{"alpha-4", "alpha-3"} {
		x.obligations = map[string][]string{removed: {"alpha-0/g"}}
		x.step()
		x.syncWorkload()
		x.step()
		if _, kept := x.claimUIDs()["data-"+removed]; !kept {
			t.Fatalf("released %s while a session needed it", removed)
		}
		x.obligations = nil
		x.step()
		if _, kept := x.claimUIDs()["data-"+removed]; kept {
			t.Fatalf("unneeded disk of %s retained", removed)
		}
	}
	x.converge()
	if replicas(x.workload()) != 3 {
		t.Fatalf("contracted to %d, want 3", replicas(x.workload()))
	}
}

// Admission webhooks (Istio, IRSA, Datadog) mutate Pods, not the template.
// None of their additions may stall rollout, contraction or settlement.
func admitMeshIRSAAndDatadog(x *operationFixture) {
	x.admit = func(p *corev1.PodSpec) {
		p.InitContainers = append(p.InitContainers, corev1.Container{Name: "istio-init", Image: "istio/proxyv2"})
		p.Containers = append([]corev1.Container{{Name: "istio-proxy", Image: "istio/proxyv2"}}, p.Containers...)
		p.Volumes = append(p.Volumes, corev1.Volume{Name: "aws-iam-token", Projected: &corev1.ProjectedVolumeSource{}})
		for i := range p.Containers {
			c := &p.Containers[i]
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "AWS_ROLE_ARN", Value: "arn:aws:iam::123456789012:role/celld"},
				corev1.EnvVar{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"},
				corev1.EnvVar{Name: "DD_AGENT_HOST", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
				corev1.EnvVar{Name: "DD_ENV", Value: "test"},
			)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "aws-iam-token", MountPath: "/var/run/secrets/eks.amazonaws.com/serviceaccount", ReadOnly: true})
		}
	}
}

func TestAdmissionMutationsDoNotStallTheLifecycle(t *testing.T) {
	for _, profile := range []string{"PersistentFleet", "Bucket"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile, admitMeshIRSAAndDatadog)
			x.converge()
			if p := x.pod("alpha-0"); p.Spec.Containers[0].Name != "istio-proxy" || len(p.Spec.InitContainers) == 0 {
				t.Fatal("fixture admission not applied")
			}
			x.edit(func(f *fleet.CelldFleet) {
				f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"}
				f.Spec.RuntimeImage = fixtureRuntimeUpgrade
			})
			x.step()
			x.syncWorkload()
			x.converge()
			if profile == "PersistentFleet" {
				for _, name := range []string{"alpha-0", "alpha-1", "alpha-2"} {
					if x.pod(name).Spec.Containers[1].Image != fixtureRuntimeUpgrade {
						t.Fatalf("%s not upgraded under admission", name)
					}
				}
			}
			x.desired(2)
			x.converge()
			if replicas(x.workload()) != 2 {
				t.Fatal("contraction stalled under admission")
			}
		})
	}
}

// A node drain evicts a member mid-rollout. The operator must not restart a
// second member while the evicted one is down, and the budget must refuse a
// further eviction until the fleet settles.
func TestDrainDuringRolloutKeepsOneDisruption(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.converge()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "r1"} })
	x.step()
	x.syncWorkload()
	if replaced := x.restartStep(); replaced != "alpha-2" {
		t.Fatalf("first restart replaced %q", replaced)
	}
	// The drain evicts outdated alpha-0; its cordoned node keeps it unscheduled.
	if err := x.r.Delete(t.Context(), x.pod("alpha-0")); err != nil {
		t.Fatal(err)
	}
	outdated := x.pod("alpha-1").UID
	for range 3 {
		x.step()
	}
	if x.pod("alpha-1").UID != outdated {
		t.Fatal("restarted a second member while an evicted one was down")
	}
	if x.budget() != 0 {
		t.Fatal("budget admits another eviction while a member is down")
	}
	// Uncordon: the StatefulSet recreates alpha-0 at the update revision.
	x.syncWorkload()
	if replaced := x.restartStep(); replaced != "alpha-1" {
		t.Fatalf("rollout did not resume with alpha-1, replaced %q", replaced)
	}
	x.converge()
	if x.budget() != 1 {
		t.Fatal("budget not reopened after the rollout settled")
	}
}
