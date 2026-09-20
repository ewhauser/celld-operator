package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEBSHandoffRequiresUniqueHealthyRWOPAttachment(t *testing.T) {
	for _, which := range []string{"valid", "old-attached", "old-deleting", "wrong-target", "not-attached", "attach-error", "detach-error", "wrong-driver", "rwo", "wrong-disk", "no-attachment", "multi-attach-capable", "unknown-class"} {
		t.Run(which, func(t *testing.T) {
			f := fixture("persistent", "bucket", "PersistentFleet")
			claim := &corev1.PersistentVolumeClaim{Name: "data-persistent-2", Namespace: f.Namespace, UID: "claim", Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
			pv := &corev1.PersistentVolume{Name: "pv", UID: "volume", Spec: corev1.PersistentVolumeSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-123"}}}}
			a := &storagev1.VolumeAttachment{Name: "new", Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: "new-host", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: new("pv")}}, Status: storagev1.VolumeAttachmentStatus{Attached: true}}
			m := persistentMember{Node: "persistent-2", Host: "new-host", ClaimUID: "claim", VolumeUID: "volume", VolumeHandle: "vol-123"}
			claim.Spec.StorageClassName = new("gp3")
			pv.Spec.StorageClassName = "gp3"
			class := &storagev1.StorageClass{Name: "gp3", Provisioner: "ebs.csi.aws.com", Parameters: map[string]string{"type": "gp3"}, VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer), ReclaimPolicy: new(corev1.PersistentVolumeReclaimRetain)}
			objects := []client.Object{f, claim, pv, class}
			switch which {
			case "multi-attach-capable":
				class.Parameters["type"] = "io2"
			case "unknown-class":
				delete(class.Parameters, "type")
			case "old-attached", "old-deleting":
				old := a.DeepCopy()
				old.Name = "old"
				old.Spec.NodeName = "old-host"
				if which == "old-deleting" {
					now := metav1.Now()
					old.DeletionTimestamp = &now
					old.Finalizers = []string{"attachment"}
				}
				objects = append(objects, old)
			case "wrong-target":
				a.Spec.NodeName = "other"
			case "not-attached":
				a.Status.Attached = false
			case "attach-error":
				a.Status.AttachError = &storagev1.VolumeError{Message: "error"}
			case "detach-error":
				a.Status.DetachError = &storagev1.VolumeError{Message: "error"}
			case "wrong-driver":
				pv.Spec.CSI.Driver = "other"
			case "rwo":
				claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			case "wrong-disk":
				pv.Spec.CSI.VolumeHandle = "wrong"
			}
			if which != "no-attachment" {
				objects = append(objects, a)
			}
			r := setup(t, objects...)
			err := r.verifyVolumeAttachment(t.Context(), f, m)
			if (err == nil) != (which == "valid") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestHandoffRequiresLatestRetiredDiskAndSameZone(t *testing.T) {
	old := persistentMember{Node: "persistent-2", PodUID: "old-pod", Generation: "old-gen", Host: "old-host", BootID: "old-boot", DiskID: "disk", Zone: "zone", Stopped: true, Retired: true, RestartDenied: true}
	pod := &corev1.Pod{Name: old.Node, UID: "new-pod"}
	node := &corev1.Node{Name: "new-host", UID: "node", Labels: map[string]string{corev1.LabelTopologyZone: "zone"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "new-boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	state := launcher.State{DiskID: "disk", PreviousHost: "old-host\nold-boot", Generation: "new-gen", BootID: "new-boot"}
	for _, which := range []string{"valid", "superseded-earlier", "superseded-latest", "not-stopped", "not-retired", "wrong-disk", "wrong-boot", "wrong-zone", "stale-predecessor", "legacy"} {
		t.Run(which, func(t *testing.T) {
			p, n, s := old, node.DeepCopy(), state
			j := &lifecycleJournal{PersistentHistory: []persistentMember{p}}
			switch which {
			case "superseded-earlier":
				// The pod was recreated once on its old host, so the earlier
				// invocation is resolved by the successor's exclusive lock.
				earlier := p
				earlier.PodUID = "earlier-pod"
				earlier.Generation = "earlier-gen"
				earlier.Stopped, earlier.Retired, earlier.RestartDenied = false, false, false
				earlier.Superseded = true
				j.PersistentHistory = append([]persistentMember{earlier}, j.PersistentHistory...)
			case "superseded-latest":
				// The predecessor the handoff is compared against must itself be
				// a positive retirement, never merely superseded.
				j.PersistentHistory[0].Stopped, j.PersistentHistory[0].Retired, j.PersistentHistory[0].RestartDenied = false, false, false
				j.PersistentHistory[0].Superseded = true
			case "not-stopped":
				j.PersistentHistory[0].Stopped = false
			case "not-retired":
				j.PersistentHistory[0].Retired = false
			case "wrong-disk":
				s.DiskID = "other"
			case "wrong-boot":
				s.BootID = "old-boot"
			case "wrong-zone":
				n.Labels[corev1.LabelTopologyZone] = "other"
			case "stale-predecessor":
				next := p
				next.Host = "intermediate"
				next.Generation = "intermediate"
				j.PersistentHistory = append(j.PersistentHistory, next)
			case "legacy":
				j.PersistentHistory[0].DiskID = ""
			}
			_, err := handoffPredecessor(j, pod, s, n)
			if (err == nil) != (which == "valid" || which == "superseded-earlier") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestTransferableDiskSchedulingDoesNotRequireOldHost(t *testing.T) {
	p := persistentSetup(t)
	p.r.Options.LocalTest = false
	old := persistentMember{Node: "persistent-2", PodUID: "old", Generation: "retired", Host: "unavailable-old-host", HostUID: "old-node", BootID: "old-boot", Zone: "us-east-1a", DiskID: "nonce", ClaimUID: "claim-persistent-2", VolumeUID: "pv-persistent-2", VolumeHandle: "vol-2", Stopped: true, Retired: true, RestartDenied: true}
	p.j.PersistentHistory = []persistentMember{old}
	claim := &corev1.PersistentVolumeClaim{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "data-persistent-2"}, claim); err != nil {
		t.Fatal(err)
	}
	volume := &corev1.PersistentVolume{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
		t.Fatal(err)
	}
	old.VolumeUID = string(volume.UID)
	p.j.PersistentHistory[0] = old
	volume.Spec.HostPath = nil
	volume.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-2"}
	if err := p.r.Update(t.Context(), volume); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: old.Node}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = ""
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	if err := p.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err := p.r.schedulePersistent(t.Context(), p.f, p.res, p.j); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.SchedulingGates) != 0 || pod.Spec.NodeSelector[corev1.LabelTopologyZone] != old.Zone || pod.Spec.NodeSelector[corev1.LabelHostname] != "" {
		t.Fatalf("incorrect scheduling constraints: %+v", pod.Spec)
	}
}

func TestPersistentMemberAuthenticatesHostBoot(t *testing.T) {
	p := persistentSetup(t)
	state := p.states["persistent-0"]
	state.BootID = "other-incarnation"
	p.states["persistent-0"] = state
	if _, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false); err == nil {
		t.Fatal("authenticated old process inherited new host incarnation")
	}
	node := &corev1.Node{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: "host-0"}, node); err != nil {
		t.Fatal(err)
	}
	state.BootID = node.Status.NodeInfo.BootID
	p.states["persistent-0"] = state
	if _, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false); err != nil {
		t.Fatalf("matching authenticated boot rejected: %v", err)
	}
}

func TestPersistentStopRequiresAuthenticatedRestartDenial(t *testing.T) {
	p := persistentSetup(t)
	p.j.Operation.TargetPod = "persistent-2"
	state := p.states["persistent-2"]
	state.Phase = "Stopped"
	state.Operation = p.j.Operation.ID
	state.RestartDenied = false
	p.states["persistent-2"] = state
	if _, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, true); err == nil {
		t.Fatal("legacy stop receipt granted durable retirement")
	}
	state.RestartDenied = true
	p.states["persistent-2"] = state
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Node == "persistent-2" && !m.RestartDenied {
			t.Fatal("authenticated denial was not retained")
		}
	}
}

// A survivor that is Ready cannot be waiting for a handoff, so steady-state
// reconciles must not spend a launcher round trip (and the reservation read its
// key lookup performs) on it; a survivor that is not Ready is still probed.
func TestHandoffProbeSkipsReadySurvivors(t *testing.T) {
	for _, which := range []string{"ready", "not-ready-running", "not-ready-waiting"} {
		t.Run(which, func(t *testing.T) {
			p := persistentSetup(t)
			for i := range 3 {
				name := fmt.Sprintf("persistent-%d", i)
				p.j.PersistentHistory = append(p.j.PersistentHistory, persistentMember{
					Node: name, PodUID: "uid-" + name, Generation: p.reader.generation[name],
					Host: fmt.Sprintf("host-%d", i), HostUID: fmt.Sprintf("host-%d", i), BootID: "boot", Zone: "us-east-1a",
				})
			}
			if which != "ready" {
				pods := &corev1.PodList{}
				if err := p.r.List(t.Context(), pods, client.InNamespace(p.f.Namespace)); err != nil {
					t.Fatal(err)
				}
				for i := range pods.Items {
					pod := &pods.Items[i]
					pod.Status.Conditions = nil
					if err := p.r.Status().Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				}
			}
			if which == "not-ready-waiting" {
				for name, s := range p.states {
					s.Phase = "WaitingForHandoff"
					p.states[name] = s
				}
			}
			reservationReads := 0
			p.r.Client = interceptor.NewClient(p.r.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, o ...client.GetOption) error {
					if _, ok := obj.(*fleet.CelldStorageReservation); ok {
						reservationReads++
					}
					return c.Get(ctx, key, obj, o...)
				},
			})
			calls := 0
			inner := p.r.launcherCall
			p.r.launcherCall = func(ctx context.Context, f *fleet.CelldFleet, pod *corev1.Pod, op, gen string) (launcher.State, error) {
				calls++
				// Mirror the reservation read the real launcherKey performs per call.
				if err := p.r.Get(ctx, client.ObjectKey{Name: reservationName(f)}, &fleet.CelldStorageReservation{}); err != nil {
					return launcher.State{}, err
				}
				return inner(ctx, f, pod, op, gen)
			}
			err := p.r.schedulePersistent(t.Context(), p.f, p.res, p.j)
			switch which {
			case "ready":
				if err != nil {
					t.Fatal(err)
				}
				if calls != 0 || reservationReads != 0 {
					t.Fatalf("idle reconcile probed Ready survivors: %d launcher calls, %d reservation reads", calls, reservationReads)
				}
			case "not-ready-running":
				if err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatalf("not-Ready survivors probed %d times, want 3", calls)
				}
			case "not-ready-waiting":
				// The probe reached the launcher and the waiting phase was acted on;
				// this fixture is LocalTest, where cross-host handoff is refused.
				if err == nil || !strings.Contains(err.Error(), "cross-host handoff requires real EBS CSI") {
					t.Fatalf("waiting launcher was not probed and authorized: %v", err)
				}
				if calls == 0 {
					t.Fatal("waiting launcher was not probed")
				}
			}
		})
	}
}
