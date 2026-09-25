package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/launcher"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type operationFixture struct {
	t        *testing.T
	r        *Reconciler
	f        *fleet.CelldFleet
	clock    time.Time
	states   map[string]launcher.State
	requests int
	cpu      int64
	// admit simulates mutating admission on each Pod the workload controller creates.
	admit func(*corev1.PodSpec)
}
type fixtureCollector struct{ x *operationFixture }

func (c fixtureCollector) Collect(ctx context.Context, _ *fleet.CelldFleet) capacity.Observation {
	x := c.x
	pods := &corev1.PodList{}
	if err := x.r.List(ctx, pods, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		x.t.Fatal(err)
	}
	out := capacity.Observation{At: x.clock, Complete: true}
	for _, p := range pods.Items {
		id, _ := podIdentity(&p)
		out.Samples = append(out.Samples, capacity.Sample{Identity: id, Ready: true, CPU: x.cpu, MemoryMiB: 10, RuntimeMemoryMiB: 10, RuntimeAt: x.clock, RuntimeReceived: x.clock, MetricsAt: x.clock, MetricsReceived: x.clock, Window: 15 * time.Second})
	}
	return out
}
func newOperationFixture(t *testing.T, profile string) *operationFixture {
	t.Helper()
	deployment := profile == "Deployment"
	if deployment {
		profile = "Bucket"
	}
	f := fixture("alpha", "bucket-alpha", profile)
	if profile == "Bucket" && !deployment {
		f.Spec.BucketWorkload = "Ordered"
	}
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	x := &operationFixture{t: t, f: f, clock: time.Now().UTC().Truncate(time.Millisecond), states: map[string]launcher.State{}, cpu: 10}
	x.r = setup(t, f)
	x.r.now = func() time.Time { return x.clock }
	x.r.Collector = fixtureCollector{x}
	x.r.launcherCall = x.launcher
	reconcile(t, x.r, f)
	reconcile(t, x.r, f)
	x.syncWorkload()
	return x
}
func (x *operationFixture) launcher(ctx context.Context, _ *fleet.CelldFleet, p *corev1.Pod, operation, generation string) (launcher.State, error) {
	s := x.states[p.Name]
	if operation != "" {
		x.requests++
		if generation != s.Generation {
			return launcher.State{}, errors.New("wrong generation")
		}
		if s.Operation != "" && s.Operation != operation {
			return launcher.State{}, errors.New("conflicting operation")
		}
		deadline, ok := ctx.Value(removalDeadlineKey{}).(time.Time)
		if !ok {
			return launcher.State{}, errors.New("missing deadline")
		}
		s.DeadlineMS = deadline.UnixMilli()
		completeLauncherRemoval(&s, operation)
		x.states[p.Name] = s
	}
	return s, nil
}
func (x *operationFixture) state() *fleetState { return getCurrentState(x.t, x.r, x.f) }
func (x *operationFixture) workload() client.Object {
	w := emptyObject(workload(x.f, x.r.Options))
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), w); err != nil {
		x.t.Fatal(err)
	}
	return w
}
func (x *operationFixture) step() { x.t.Helper(); reconcile(x.t, x.r, x.f) }
func (x *operationFixture) until(phase string) {
	x.t.Helper()
	for range 35 {
		x.step()
		s := x.state()
		if s.Operation != nil && s.Operation.Phase == phase {
			return
		}
	}
	x.t.Fatalf("phase %s not reached: %+v, status %+v", phase, x.state(), reconcile(x.t, x.r, x.f).Status)
}
func (x *operationFixture) desired(n int32) { x.f = desiredCount(x.t, x.r, x.f, n) }
func (x *operationFixture) edit(edit func(*fleet.CelldFleet)) {
	x.t.Helper()
	f := &fleet.CelldFleet{}
	if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(x.f), f); err != nil {
		x.t.Fatal(err)
	}
	edit(f)
	if err := x.r.Update(x.t.Context(), f); err != nil {
		x.t.Fatal(err)
	}
	x.f = f
}
func (x *operationFixture) finish() {
	x.t.Helper()
	for range 60 {
		x.step()
		x.syncWorkload()
		if x.state().Operation == nil {
			return
		}
	}
	x.t.Fatalf("operation never completed: %+v status %+v", x.state().Operation, reconcile(x.t, x.r, x.f).Status)
}

// Simulate only the Kubernetes workload controller, scheduler and CSI binding.
// Runtime proof always travels separately through the authenticated-client seam.
func (x *operationFixture) syncWorkload() {
	t := x.t
	ctx := t.Context()
	x.syncStorage()
	w := x.workload()
	n := replicas(w)
	pods := &corev1.PodList{}
	if err := x.r.List(ctx, pods, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		var ordinal int
		_, _ = fmt.Sscanf(p.Name, x.f.Name+"-%d", &ordinal)
		if ordinal >= int(n) {
			if err := x.r.Delete(ctx, &p, client.Preconditions{UID: &p.UID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range n {
		name := fmt.Sprintf("%s-%d", x.f.Name, i)
		p := &corev1.Pod{}
		err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: name}, p)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		host := fmt.Sprintf("host-%d", i)
		node := &corev1.Node{Name: host, UID: types.UID(host), Labels: map[string]string{corev1.LabelTopologyZone: "us-east-1a", corev1.LabelHostname: host}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if err := x.r.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		spec := podTemplate(x.f, x.r.Options).Spec
		spec.SchedulingGates = nil
		spec.NodeName = host
		owner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: x.f.Name, UID: w.GetUID(), Controller: new(true)}
		switch w := w.(type) {
		case *appsv1.StatefulSet:
			spec.Containers[0].Image = w.Spec.Template.Spec.Containers[0].Image
		case *appsv1.Deployment:
			spec.Containers[0].Image = w.Spec.Template.Spec.Containers[0].Image
			rs := &appsv1.ReplicaSet{Name: x.f.Name + "-rs", Namespace: x.f.Namespace, UID: types.UID(x.f.Name + "-rs"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: x.f.Name, UID: w.UID, Controller: new(true)}}}
			if err := x.r.Create(ctx, rs); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
			owner.Kind = "ReplicaSet"
			owner.Name = rs.Name
			owner.UID = rs.UID
		}
		if x.f.Spec.Profile == "PersistentFleet" {
			claim := &corev1.PersistentVolumeClaim{}
			err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: "data-" + name}, claim)
			if apierrors.IsNotFound(err) {
				claim = initialClaims(x.f, workload(x.f, x.r.Options))[0]
				claim.Name = "data-" + name
				claim.UID = types.UID(fmt.Sprintf("claim-%s-%d", name, x.clock.UnixNano()))
				if err := x.r.Create(ctx, claim); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if claim.Spec.VolumeName == "" {
				claim.Spec.VolumeName = "pv-" + string(claim.UID)
				if err := x.r.Update(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			claim.Status.Phase = corev1.ClaimBound
			if err := x.r.Status().Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
			pv := &corev1.PersistentVolume{Name: claim.Spec.VolumeName, UID: types.UID("volume-" + string(claim.UID)), Finalizers: []string{csiDeletionFinalizer}, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "ebs.csi.aws.com"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete, StorageClassName: x.f.Spec.Storage.StorageClassName, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-" + string(claim.UID)}}}}
			if err := x.r.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}})
		}
		if x.admit != nil {
			x.admit(&spec)
		}
		uid := types.UID(fmt.Sprintf("%s-%d", name, x.clock.UnixNano()))
		p = &corev1.Pod{Name: name, Namespace: x.f.Namespace, UID: uid, Labels: labels(x.f), OwnerReferences: []metav1.OwnerReference{owner}, Spec: spec, Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.0.0.%d", i+1), ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-" + string(uid), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(x.clock.Add(-time.Hour))}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		if err := x.r.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		x.states[name] = launcher.State{PodUID: string(uid), Node: runtimeNode(x.f, p), Host: host, BootID: "boot", DiskID: "disk-" + string(uid), Invocation: "invocation-" + string(uid), Generation: "generation-" + string(uid), PID: 42, Phase: "Running"}
	}
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		w.Status.ReadyReplicas = n
		w.Status.Replicas = n
		w.Status.ObservedGeneration = w.Generation
	case *appsv1.Deployment:
		w.Status.ReadyReplicas = n
		w.Status.Replicas = n
		w.Status.ObservedGeneration = w.Generation
	}
	if err := x.r.Status().Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	x.clock = x.clock.Add(time.Second)
}
func TestStrictRemovalBothProfiles(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile)
			x.desired(2)
			x.until("Intent")
			if x.requests != 0 {
				t.Fatal("runtime called before intent")
			}
			x.until("Requesting")
			if x.requests != 0 {
				t.Fatal("runtime called in issuance CAS")
			}
			x.until("ProofCaptured")
			o := x.state().Operation
			proof := o.Targets[0].Proof
			if proof == nil || !validProof(o, o.Targets[0], *proof) {
				t.Fatal("missing exact proof")
			}
			if replicas(x.workload()) != 3 {
				t.Fatal("effect preceded durable proof")
			}
			disk := o.Targets[0].Storage
			x.r.launcherCall = func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error) {
				t.Fatal("proof replay read launcher")
				return launcher.State{}, nil
			}
			x.finish()
			s := x.state()
			if s.Applied != 2 || s.Operation != nil || s.Completion == nil {
				t.Fatalf("not completed %+v", s)
			}
			if profile == "PersistentFleet" {
				pv := &corev1.PersistentVolume{}
				if err := x.r.Get(t.Context(), client.ObjectKey{Name: disk.Volume}, pv); !apierrors.IsNotFound(err) {
					t.Fatal("CSI disk cleanup did not complete", err)
				}
				if len(s.Claims) != 2 {
					t.Fatal("removed claim identity retained")
				}
			}
			b, _ := json.Marshal(s)
			if strings.Contains(string(b), proof.Removal.Generation) || strings.Contains(string(b), "Removal") {
				t.Fatal("completed runtime proof retained")
			}
		})
	}
}
func TestRemovalRejectsIncompleteAndWrongProof(t *testing.T) {
	for _, scenario := range []string{"accepted", "exit zero", "runtime only", "lock missing", "restart allowed", "wrong generation", "wrong pod", "wrong operation", "wrong disk", "wrong deadline", "failed", "launcher lost"} {
		t.Run(scenario, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.desired(2)
			x.until("Requesting")
			base := x.r.launcherCall
			x.r.launcherCall = func(ctx context.Context, f *fleet.CelldFleet, p *corev1.Pod, op, gen string) (launcher.State, error) {
				s, e := base(ctx, f, p, op, gen)
				switch scenario {
				case "accepted":
					s.Phase = "Stopping"
					s.Removal.Phase = "draining"
					s.Removal.DataSafe = false
				case "exit zero":
					s.Removal = launcher.RemovalResult{}
				case "runtime only":
					s.ChildExited = false
				case "lock missing":
					s.InheritedLockReleased = false
				case "restart allowed":
					s.RestartDenied = false
				case "wrong generation":
					s.Generation = "other"
				case "wrong pod":
					s.PodUID = "other"
				case "wrong operation":
					s.Operation = "other"
				case "wrong disk":
					s.DiskID = "other"
				case "wrong deadline":
					s.DeadlineMS++
				case "failed":
					s.Phase = "Failed"
					s.Error = "runtime failed"
				case "launcher lost":
					return launcher.State{}, errors.New("launcher lost")
				}
				return s, e
			}
			for range 4 {
				x.step()
			}
			if replicas(x.workload()) != 3 || x.state().Operation == nil {
				t.Fatal("incomplete proof authorized removal")
			}
			if scenario == "failed" {
				x.r.launcherCall = base
				for range 4 {
					x.step()
				}
				if replicas(x.workload()) != 3 || x.state().Operation.Phase != "Blocked" {
					t.Fatal("later exit repaired a failed result")
				}
			}
		})
	}
}
func TestCancellationReversalPauseAndNewRequests(t *testing.T) {
	for _, phase := range []string{"Intent", "Requesting", "ProofCaptured"} {
		for _, change := range []string{"reverse", "pause", "restart", "upgrade"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				x := newOperationFixture(t, "Bucket")
				x.desired(2)
				x.until(phase)
				id := x.state().Operation.ID
				x.edit(func(f *fleet.CelldFleet) {
					switch change {
					case "reverse":
						f.Spec.Replicas = 3
					case "pause":
						f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
					case "restart":
						f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "new-request", AllowCoordinatedDowntime: true}
					case "upgrade":
						f.Spec.RuntimeImage = "ghcr.io/ewhauser/celld@sha256:" + strings.Repeat("c", 64)
					}
				})
				if phase == "Intent" {
					x.step()
					s := x.state()
					if s.Operation != nil || s.Completion.ID != id || s.Completion.Outcome != "CanceledBeforeIssue" || x.requests != 0 || replicas(x.workload()) != 3 {
						t.Fatal("unissued cancellation failed", s)
					}
				} else {
					x.finish()
					s := x.state()
					if s.Applied != 2 || s.Completion.ID != id {
						t.Fatal("issued target retargeted", s)
					}
				}
			})
		}
	}
}
func TestDeadlineNeverMakesIssuanceUnissued(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	x.until("Requesting")
	deadline := x.state().Operation.Deadline
	x.clock = deadline.Add(time.Hour)
	x.desired(3)
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
	for range 5 {
		x.step()
	}
	s := x.state()
	if s.Operation == nil || s.Operation.Deadline != deadline || replicas(x.workload()) != 3 || s.Completion != nil {
		t.Fatal("expired uncertain issuance canceled or completed", s)
	}
}
func TestIdentityDriftBlocksProofAndEffect(t *testing.T) {
	for _, phase := range []string{"Requesting", "ProofCaptured"} {
		for _, what := range []string{"workload", "pod", "host", "claim", "volume", "handle"} {
			t.Run(phase+"/"+what, func(t *testing.T) {
				x := newOperationFixture(t, "PersistentFleet")
				x.desired(2)
				x.until(phase)
				o := x.state().Operation
				target := o.Targets[0]
				ctx := t.Context()
				switch what {
				case "workload":
					w := x.workload()
					w.SetUID("replacement")
					if err := x.r.Update(ctx, w); err != nil {
						t.Fatal(err)
					}
				case "pod":
					p := &corev1.Pod{}
					if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: target.Pod}, p); err != nil {
						t.Fatal(err)
					}
					p.UID = "replacement"
					if err := x.r.Update(ctx, p); err != nil {
						t.Fatal(err)
					}
				case "host":
					n := &corev1.Node{}
					if err := x.r.Get(ctx, client.ObjectKey{Name: target.Identity.Host}, n); err != nil {
						t.Fatal(err)
					}
					n.UID = "replacement"
					if err := x.r.Update(ctx, n); err != nil {
						t.Fatal(err)
					}
				case "claim":
					c := &corev1.PersistentVolumeClaim{}
					if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: target.Storage.Claim}, c); err != nil {
						t.Fatal(err)
					}
					c.UID = "replacement"
					if err := x.r.Update(ctx, c); err != nil {
						t.Fatal(err)
					}
				default:
					p := &corev1.PersistentVolume{}
					if err := x.r.Get(ctx, client.ObjectKey{Name: target.Storage.Volume}, p); err != nil {
						t.Fatal(err)
					}
					if what == "volume" {
						p.UID = "replacement"
					} else {
						p.Spec.CSI.VolumeHandle = "replacement"
					}
					if err := x.r.Update(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
				for range 4 {
					x.step()
				}
				if replicas(x.workload()) != 3 || x.state().Operation == nil {
					t.Fatal("drift authorized effect")
				}
			})
		}
	}
}
func TestLostResponsesAndControllerRestarts(t *testing.T) {
	for _, boundary := range []string{"intent", "requesting", "proof", "effect", "cleanup", "completion"} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lost=%v", boundary, lost), func(t *testing.T) {
				x := newOperationFixture(t, "PersistentFleet")
				x.desired(2)
				base := x.r.Client
				triggered := false
				x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					hit := false
					switch obj := obj.(type) {
					case *fleet.CelldStorageReservation:
						s, _ := readState(obj)
						if s != nil {
							if o := s.Operation; o != nil {
								hit = boundary == "intent" && o.Phase == "Intent" || boundary == "requesting" && o.Phase == "Requesting" || boundary == "proof" && len(o.Targets) > 0 && o.Targets[0].Proof != nil
							} else {
								hit = boundary == "completion" && s.Completion != nil
							}
						}
					case *appsv1.StatefulSet:
						hit = boundary == "effect" && replicas(obj) == 2
					}
					if hit && !triggered {
						triggered = true
						if lost {
							if err := c.Update(ctx, obj, opts...); err != nil {
								return err
							}
						}
						return errors.New("injected response loss")
					}
					return c.Update(ctx, obj, opts...)
				}, Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					_, claim := obj.(*corev1.PersistentVolumeClaim)
					if boundary == "cleanup" && claim && !triggered {
						triggered = true
						if lost {
							if err := c.Delete(ctx, obj, opts...); err != nil {
								return err
							}
						}
						return errors.New("injected response loss")
					}
					return c.Delete(ctx, obj, opts...)
				}})
				for range 70 {
					_, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)})
					if err != nil && !strings.Contains(err.Error(), "injected") {
						t.Fatal(err)
					}
					x.syncWorkload()
					if triggered && x.state().Operation == nil && x.state().Completion != nil {
						break
					}
				}
				if !triggered || x.state().Operation != nil || x.state().Applied != 2 {
					t.Fatalf("boundary not recovered %v %+v", triggered, x.state())
				}
			})
		}
	}
}
func TestStaleReservationAndWorkloadWriters(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.desired(2)
	x.until("Intent")
	res := &fleet.CelldStorageReservation{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Name: reservationName(x.f)}, res); err != nil {
		t.Fatal(err)
	}
	stale := x.r.hydrate(t.Context(), res)
	x.desired(3)
	x.step()
	stale.j.Operation.Phase = "Requesting"
	if err := x.r.saveState(t.Context(), stale.res, stale.j); !apierrors.IsConflict(err) {
		t.Fatal("stale issuance won cancellation", err)
	}
	x.desired(2)
	x.until("Apply")
	oldW := x.workload()
	oldH := x.r.hydrate(t.Context(), envReservation(t, x.r, x.f))
	x.finish()
	setReplicas(oldW, oldH.j.Operation.To)
	oldW.GetAnnotations()[operationKey] = effectID(oldH.j.Operation, false)
	if err := x.r.Update(t.Context(), oldW); !apierrors.IsConflict(err) {
		t.Fatal("delayed workload writer won", err)
	}
}
func envReservation(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *fleet.CelldStorageReservation {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	return res
}
func TestCurrentAuthorityBoundedAndStatusRebuildable(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	maxSize := 0
	for i := range 8 {
		x.desired(2)
		x.until("Intent")
		x.finish()
		x.r.launcherCall = x.launcher
		x.desired(3)
		x.until("Intent")
		x.finish()
		s := x.state()
		b, _ := json.Marshal(s)
		maxSize = max(maxSize, len(b))
		if len(s.Claims) != 3 || len(b) > 3000 {
			t.Fatalf("authority accumulated after %d operations: %s", i, b)
		}
	}
	x.edit(func(f *fleet.CelldFleet) { f.Status.Lifecycle.OperationID = "edited" })
	f := &fleet.CelldFleet{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), f); err != nil {
		t.Fatal(err)
	}
	f.Status = fleet.CelldFleetStatus{AppliedReplicas: 99, Lifecycle: fleet.LifecycleStatus{OperationID: "fabricated", LastOutcome: "Success"}}
	if err := x.r.Status().Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	x.step()
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), f); err != nil {
		t.Fatal(err)
	}
	if f.Status.AppliedReplicas != 3 || f.Status.Lifecycle.OperationID != "" || f.Status.Lifecycle.LastOutcome != x.state().Completion.Outcome {
		t.Fatal("editable status used as authority", f.Status)
	}
	t.Logf("maximum completed authority size: %d bytes", maxSize)
}
func TestMaintenanceUsesCurrentWorkingSet(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, kind := range []string{"Restart", "Upgrade"} {
			t.Run(profile+"/"+kind, func(t *testing.T) {
				x := newOperationFixture(t, profile)
				x.edit(func(f *fleet.CelldFleet) {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "request-1", AllowCoordinatedDowntime: true}
					if kind == "Upgrade" {
						f.Spec.RuntimeImage = "ghcr.io/ewhauser/celld@sha256:" + strings.Repeat("c", 64)
					}
				})
				x.until("Intent")
				if len(x.state().Operation.Targets) != 3 {
					t.Fatal("maintenance working set incomplete")
				}
				x.finish()
				s := x.state()
				if s.Applied != 3 || s.RestartToken != "request-1" || s.Operation != nil || s.RuntimeImage != x.f.Spec.RuntimeImage {
					t.Fatal("maintenance incomplete", s)
				}
				calls := x.requests
				for range 3 {
					x.step()
				}
				if x.requests != calls {
					t.Fatal("request token replayed")
				}
			})
		}
	}
}
func TestNoRecoveryMetadataDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"internal/runtime/v050", "internal/runtime/v041", "internal/runtime/catalog", "internal/fencing", "ProductionEvidence", "lifecycleJournal"} {
			if strings.Contains(string(b), forbidden) {
				t.Errorf("%s retains runtime recovery authority %s", entry.Name(), forbidden)
			}
		}
	}
}

func TestLauncherCrashCaptureBoundary(t *testing.T) {
	for _, captured := range []bool{false, true} {
		t.Run(fmt.Sprint(captured), func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.desired(2)
			x.until("Requesting")
			if captured {
				x.until("ProofCaptured")
			}
			target := x.state().Operation.Targets[0]
			p := &corev1.Pod{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: target.Pod}, p); err != nil {
				t.Fatal(err)
			}
			p.Status.ContainerStatuses[0].RestartCount++
			p.Status.ContainerStatuses[0].ContainerID = "successor-launcher"
			if err := x.r.Status().Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			x.r.launcherCall = func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error) {
				return launcher.State{}, errors.New("launcher positive result lost")
			}
			if captured {
				x.finish()
				if x.state().Applied != 2 {
					t.Fatal("durable proof did not survive launcher crash")
				}
			} else {
				for range 5 {
					x.step()
				}
				if replicas(x.workload()) != 3 || x.state().Operation == nil {
					t.Fatal("lost proof inferred from successor")
				}
			}
		})
	}
}
func TestStateRejectsUnboundOrOversizedAuthority(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.desired(2)
	x.until("ProofCaptured")
	res := envReservation(t, x.r, x.f)
	for _, change := range []string{"version", "fleet", "workload", "deadline", "phase", "target", "proof", "history", "oversized"} {
		t.Run(change, func(t *testing.T) {
			snapshot := res.DeepCopy()
			s, err := readState(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "version":
				s.Version++
			case "fleet":
				s.Operation.FleetUID = "other"
			case "workload":
				s.Operation.WorkloadUID = "other"
			case "deadline":
				s.Operation.Deadline = s.Operation.StartedAt
			case "phase":
				s.Operation.Phase = "ForceComplete"
			case "target":
				s.Operation.Targets = nil
			case "proof":
				s.Operation.Targets[0].Proof.RestartDenied = false
			case "oversized":
				s.Operation.Blocker = strings.Repeat("x", maxStateBytes)
			}
			b, _ := json.Marshal(s)
			snapshot.Annotations[stateKey] = string(b)
			if change == "history" {
				snapshot.Annotations[stateKey] = strings.TrimSuffix(string(b), "}") + `,"History":[]}`
			}
			if _, err := readState(snapshot); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}
func TestNoRuntimeProofFromAbsentPodOrReadiness(t *testing.T) {
	for _, what := range []string{"absent", "unready", "terminated"} {
		t.Run(what, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			x.desired(2)
			x.until("Requesting")
			target := x.state().Operation.Targets[0]
			p := &corev1.Pod{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: target.Pod}, p); err != nil {
				t.Fatal(err)
			}
			switch what {
			case "absent":
				if err := x.r.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "unready":
				p.Status.Conditions = nil
				if err := x.r.Status().Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "terminated":
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
				if err := x.r.Status().Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			x.r.launcherCall = func(context.Context, *fleet.CelldFleet, *corev1.Pod, string, string) (launcher.State, error) {
				return launcher.State{}, errors.New("unavailable")
			}
			for range 3 {
				x.step()
			}
			if replicas(x.workload()) != 3 || x.state().Operation == nil {
				t.Fatal("absence/readiness/exit used as proof")
			}
		})
	}
}

func TestDeploymentBucketMaintenanceAndContractionBoundary(t *testing.T) {
	x := newOperationFixture(t, "Deployment")
	x.desired(2)
	for range 3 {
		x.step()
	}
	if x.state().Operation != nil || x.requests != 0 || replicas(x.workload()) != 3 {
		t.Fatal("arbitrary deployment victim admitted")
	}
	x.desired(3)
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "restart", AllowCoordinatedDowntime: true}
	})
	x.until("Intent")
	x.finish()
	if x.state().RestartToken != "restart" || x.state().Applied != 3 {
		t.Fatal("deployment working set failed")
	}
}
func TestDeleteUsesStrictWorkingSetAndDisposesDisks(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile)
			if err := x.r.Delete(t.Context(), x.f); err != nil {
				t.Fatal(err)
			}
			x.until("Intent")
			if x.state().Operation.Kind != "Delete" || len(x.state().Operation.Targets) != 3 {
				t.Fatal("delete did not capture all targets")
			}
			x.finish()
			if x.state().Applied != 0 || x.state().Completion.Kind != "Delete" {
				t.Fatal("delete operation incomplete")
			}
			for range 3 {
				if _, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)}); err != nil {
					t.Fatal(err)
				}
			}
			got := &fleet.CelldFleet{}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), got); !apierrors.IsNotFound(err) {
				t.Fatal("finalizer not released", err)
			}
			res := envReservation(t, x.r, x.f)
			if res.Spec.FleetUID != string(x.f.UID) {
				t.Fatal("bucket reservation released")
			}
			if profile == "PersistentFleet" {
				pvs := &corev1.PersistentVolumeList{}
				if err := x.r.List(t.Context(), pvs); err != nil {
					t.Fatal(err)
				}
				if len(pvs.Items) != 0 {
					t.Fatal("CSI disks not deleted")
				}
			}
		})
	}
}

func TestHundredMemberMaintenanceFitsBound(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	x.until("ProofCaptured")
	s := x.state()
	o := s.Operation
	o.Kind = "Restart"
	o.Phase = "DeleteClaims"
	o.From = 100
	o.To = 100
	s.Applied = 100
	base := o.Targets[0]
	s.Capacity = &capacity.State{Load: map[string]capacity.Load{}, Stamps: map[string]capacity.Stamp{}, Addition: &capacity.Addition{Before: map[string]capacity.Load{}}, IneffectiveBatches: 1}
	o.Targets = nil
	s.Claims = map[string]types.UID{}
	for i := range 100 {
		// Realistic UUIDs, 64-hex runtime/container IDs, maximum fleet name length,
		// a full CSI volume handle, and the independently captured terminal result.
		t := base
		t.Pod = fmt.Sprintf("%s-%d", strings.Repeat("f", 40), i)
		t.PodUID = types.UID(fmt.Sprintf("01234567-0123-4567-89ab-%012d", i))
		t.HostUID = fmt.Sprintf("11234567-0123-4567-89ab-%012d", i)
		t.Container = string(t.PodUID) + "/containerd://" + strings.Repeat("a", 64) + "/0"
		t.Identity = processIdentity{Node: t.Pod, Host: "ip-10-123-123-123.ec2.internal", BootID: "21234567-0123-4567-89ab-012345678901", Invocation: strings.Repeat("b", 64), Generation: strings.Repeat("c", 64), DiskID: strings.Repeat("d", 64), PID: 123456}
		t.Storage = &volumeIdentity{Claim: "data-" + t.Pod, ClaimUID: types.UID(fmt.Sprintf("31234567-0123-4567-89ab-%012d", i)), ClaimVersion: "12345678901234", Volume: "pvc-" + string(t.PodUID), VolumeUID: types.UID(fmt.Sprintf("41234567-0123-4567-89ab-%012d", i)), Handle: "ebs.csi.aws.com:vol-0123456789abcdef0", DeletionProtected: true, CleanupStarted: true}
		t.Proof = &operationProof{Removal: launcher.RemovalResult{Operation: o.ID, Generation: t.Identity.Generation, Mode: "remove-disk", Phase: "data_safe", ControlOnly: true, DataSafe: true}, ChildExited: true, InheritedLockReleased: true, RestartDenied: true}
		o.Targets = append(o.Targets, t)
		s.Claims[t.Storage.Claim] = t.Storage.ClaimUID
		s.Capacity.Load[t.Container] = capacity.Load{CPU: 1000, MemoryMiB: 1024, Pressure: true}
		s.Capacity.Addition.Before[t.Container] = s.Capacity.Load[t.Container]
		s.Capacity.Stamps[t.Container] = capacity.Stamp{Runtime: o.StartedAt, Metrics: o.StartedAt}
	}
	resetMaintenanceCapacity(s)
	if len(s.Capacity.Load) != 0 || len(s.Capacity.Stamps) != 0 || len(s.Capacity.Addition.Before) != 100 || s.Capacity.IneffectiveBatches != 1 {
		t.Fatal("disruption erased capacity hold or retained stale samples")
	}
	if err := validateState(s); err != nil {
		t.Fatal(err)
	}
	res := envReservation(t, x.r, x.f)
	if err := x.r.saveState(t.Context(), res, s); err != nil {
		t.Fatal(err)
	}
	b := res.Annotations[stateKey]
	if len(b) > maxStateBytes {
		t.Fatalf("100-member maintenance exceeds bound: %d", len(b))
	}
	reloaded, err := readState(res)
	if err != nil || len(reloaded.Operation.Targets) != 100 {
		t.Fatal("largest operation did not round-trip", err)
	}
	t.Logf("100 captured strict proofs plus current claim identities: %d/%d bytes", len(b), maxStateBytes)
}

func TestEffectCannotSweepUncapturedPods(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet", "Deployment"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile)
			if profile == "Deployment" {
				x.edit(func(f *fleet.CelldFleet) {
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "restart", AllowCoordinatedDowntime: true}
				})
			} else {
				x.desired(2)
			}
			x.until("ProofCaptured")
			original := &corev1.Pod{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "alpha-0"}, original); err != nil {
				t.Fatal(err)
			}
			if err := x.r.Delete(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			unexpected := original.DeepCopy()
			unexpected.Name = "alpha-99"
			unexpected.UID = "uncaptured"
			unexpected.ResourceVersion = ""
			if err := x.r.Create(t.Context(), unexpected); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				x.step()
			}
			if replicas(x.workload()) != 3 {
				t.Fatal("effect swept an unproven pod")
			}
		})
	}
}

func TestMissingDeterministicTargetCannotBeAdopted(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	p := &corev1.Pod{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "alpha-2"}, p); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	p.Name = "alpha-02"
	p.UID = "unexpected"
	p.ResourceVersion = ""
	if err := x.r.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	x.desired(2)
	x.step()
	if x.state().Operation != nil || x.requests != 0 || replicas(x.workload()) != 3 {
		t.Fatal("arbitrary pod adopted as deterministic victim")
	}
}
