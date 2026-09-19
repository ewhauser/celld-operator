package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/launcher"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type persistentReader struct {
	now                                             time.Time
	stopped, barrier, loss, partial, missing, noLog bool
	generation                                      map[string]string
}

func (p *persistentReader) List(_ context.Context, prefix, _ string) (v050.Page, error) {
	keys := []string{}
	if prefix == "nodes/" {
		for i := range 3 {
			if p.missing && i == 2 {
				continue
			}
			keys = append(keys, fmt.Sprintf("nodes/persistent-%d.json", i))
		}
	} else if p.loss {
		keys = append(keys, "log/historical/generation.e1.loss.json")
	}
	return v050.Page{Keys: keys, Complete: !p.partial}, nil
}
func (p *persistentReader) Get(_ context.Context, key string) ([]byte, error) {
	node := strings.TrimSuffix(strings.TrimPrefix(key, "nodes/"), ".json")
	epoch := 1
	ensemble := []string{"persistent-2"}
	state := "open"
	expires := p.now.Add(time.Minute).UnixMilli()
	if p.barrier {
		epoch = 2
		ensemble = []string{"persistent-0"}
	}
	if node == "persistent-2" && p.stopped {
		state = "sealed"
		expires = p.now.Add(-time.Second).UnixMilli()
	}
	body := map[string]any{"node": node, "ownership_index_generation": p.generation[node], "peer_protocol": 5, "expires_ms": expires, "log": map[string]any{"active": true, "state": state, "epoch": epoch, "tiered": 0, "ensemble": ensemble}}
	if p.noLog {
		delete(body, "log")
	}
	return json.Marshal(body)
}

type persistentFixture struct {
	r      *Reconciler
	f      *fleet.CelldFleet
	j      *lifecycleJournal
	res    *fleet.CelldStorageReservation
	w      *appsv1.StatefulSet
	reader *persistentReader
	states map[string]launcher.State
}

func persistentSetup(t *testing.T) *persistentFixture {
	t.Helper()
	f := fixture("persistent", "persistent-data", "PersistentFleet")
	f.Spec.Replicas = 2
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	opts := Options{LocalTest: true, LauncherImage: "local-launcher", OperatorNamespace: "celld-system"}
	reader := &persistentReader{now: time.Unix(10000, 0), generation: map[string]string{}}
	w := workload(f, opts).(*appsv1.StatefulSet)
	w.UID = "workload"
	setReplicas(w, 3)
	j := &lifecycleJournal{Version: 6, RuntimeImage: Image, Initial: 3, Applied: 3, WorkloadUID: w.UID, Claims: map[string]types.UID{}, Operation: &lifecycleOperation{ID: "removal", Phase: "Blocked", From: 3, To: 2, StartedAt: reader.now, Deadline: reader.now.Add(operationBudget)}}
	objects := []client.Object{f, w}
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
		claim := &corev1.PersistentVolumeClaim{Name: "data-" + name, Namespace: f.Namespace, UID: types.UID("claim-" + name), Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}
		claim.Spec.VolumeName = "pv-" + name
		claim.Status.Phase = corev1.ClaimBound
		pv := &corev1.PersistentVolume{Name: claim.Spec.VolumeName, UID: types.UID("pvuid-" + name), Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Name: claim.Name, Namespace: claim.Namespace, UID: claim.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/test/" + name}}}}
		objects = append(objects, pv)
		j.Claims[claim.Name] = claim.UID
		id, _ := podIdentity(pod)
		j.Inventory.Sessions = append(j.Inventory.Sessions, RuntimeSession{Node: name, Generation: gen, Container: id, Current: true, Epoch: 1})
		node := &corev1.Node{Name: host, UID: types.UID(host), Labels: map[string]string{corev1.LabelHostname: host, corev1.LabelTopologyZone: "us-east-1a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		states[name] = launcher.State{PodUID: string(pod.UID), Node: name, Host: host, Invocation: "invocation-" + name, Generation: gen, Phase: "Running"}
		objects = append(objects, pod, node, claim)
	}
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	objects = append(objects, res)
	r := setup(t, objects...)
	r.Options = opts
	r.now = func() time.Time { return reader.now }
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return reader, nil }}
	r.launcherCall = func(_ context.Context, _ *fleet.CelldFleet, p *corev1.Pod, op, gen string) (launcher.State, error) {
		s := states[p.Name]
		if op != "" {
			if gen != s.Generation {
				return s, errors.New("generation differs")
			}
			s.Phase = "Stopped"
			s.Operation = op
			states[p.Name] = s
			reader.stopped = true
		}
		return s, nil
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
		t.Fatal(err)
	}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	return &persistentFixture{r: r, f: f, j: j, res: res, w: w, reader: reader, states: states}
}
func (p *persistentFixture) step(t *testing.T) {
	t.Helper()
	if e := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.res), p.res); e != nil {
		t.Fatal(e)
	}
	var e error
	p.j, e = readJournal(p.res)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.r.Get(t.Context(), client.ObjectKeyFromObject(p.w), p.w); e != nil {
		t.Fatal(e)
	}
	pods := &corev1.PodList{}
	if e = p.r.List(t.Context(), pods); e != nil {
		t.Fatal(e)
	}
	o := capacity.Observation{At: p.reader.now, Complete: true}
	for i := range pods.Items {
		id, _ := podIdentity(&pods.Items[i])
		o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: p.reader.now, RuntimeReceived: p.reader.now, MetricsAt: p.reader.now, MetricsReceived: p.reader.now, Window: 15 * time.Second})
	}
	p.r.Collector = bucketCapacityCollector{o}
	if _, _, e = p.r.contractPersistent(t.Context(), p.f, p.res, p.j, p.w); e != nil {
		t.Fatal(e)
	}
}
func TestPersistentGracefulContractionNeedsFollowerBarrier(t *testing.T) {
	p := persistentSetup(t)
	p.step(t)
	if p.j.Operation.Phase != "Intent" {
		t.Fatal(p.j.Operation)
	}
	p.step(t)
	if p.j.Operation.Phase != "Stopping" {
		t.Fatal(p.j.Operation)
	}
	p.step(t)
	if replicas(p.w) != 3 || p.j.Operation.Phase != "Stopping" {
		t.Fatal("retired disk before follower obligations resolved")
	}
	p.reader.barrier = true
	p.step(t)
	if p.j.Operation.Phase != "Retiring" {
		t.Fatal(p.j.Operation)
	}
	p.step(t)
	if replicas(p.w) != 2 {
		t.Fatal("no actual decrement")
	}
	pod := &corev1.Pod{}
	if e := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, pod); e != nil {
		t.Fatal(e)
	}
	if e := p.r.Delete(t.Context(), pod); e != nil {
		t.Fatal(e)
	}
	p.step(t)
	p.reader.now = p.reader.now.Add(11 * time.Second)
	p.step(t)
	if p.j.Operation != nil || p.j.Applied != 2 {
		t.Fatal("recovery did not finish")
	}
	retired := p.j.PersistentHistory[slices.IndexFunc(p.j.PersistentHistory, func(m persistentMember) bool { return m.Node == "persistent-2" })]
	if !retired.Retired || !retired.Stopped {
		t.Fatal("positive retirement not retained")
	}
	if e := validateReactivation(p.j, 2, 3, p.f.Name); e != nil {
		t.Fatal(e)
	}
	// A fresh gated successor is constrained to the recorded host incarnation.
	pod.UID = "replacement"
	pod.ResourceVersion = ""
	pod.Spec.NodeName = ""
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	if e := p.r.Create(t.Context(), pod); e != nil {
		t.Fatal(e)
	}
	if e := p.r.schedulePersistent(t.Context(), p.f, p.j); e != nil {
		t.Fatal(e)
	}
	if e := p.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); e != nil {
		t.Fatal(e)
	}
	if len(pod.Spec.SchedulingGates) != 0 || pod.Spec.NodeSelector[corev1.LabelHostname] != retired.Hostname {
		t.Fatal("reuse not restricted to same host")
	}
}
func TestPersistentEvidenceFailuresNeverRemove(t *testing.T) {
	for _, fault := range []string{"loss", "partial", "missing", "log-disappeared", "generation", "node-reboot", "pvc-replaced", "launcher-replaced", "container-restarted"} {
		t.Run(fault, func(t *testing.T) {
			p := persistentSetup(t)
			p.step(t)
			p.step(t)
			switch fault {
			case "container-restarted":
				pod := &corev1.Pod{}
				_ = p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, pod)
				pod.Status.ContainerStatuses[0].RestartCount = 1
				_ = p.r.Status().Update(t.Context(), pod)
			case "log-disappeared":
				p.reader.noLog = true
			case "loss":
				p.reader.loss = true
			case "partial":
				p.reader.partial = true
			case "missing":
				p.reader.missing = true
			case "generation":
				p.reader.generation["persistent-0"] = "other"
			case "launcher-replaced":
				s := p.states["persistent-2"]
				s.Invocation = "other"
				p.states["persistent-2"] = s
			case "node-reboot":
				n := &corev1.Node{}
				_ = p.r.Get(t.Context(), client.ObjectKey{Name: "host-0"}, n)
				n.Status.NodeInfo.BootID = "rebooted"
				_ = p.r.Status().Update(t.Context(), n)
			case "pvc-replaced":
				c := &corev1.PersistentVolumeClaim{}
				_ = p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "data-persistent-2"}, c)
				c.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
				_ = p.r.Update(t.Context(), c)
			}
			p.reader.barrier = true
			p.step(t)
			if replicas(p.w) != 3 || (p.j.Operation.Phase != "Stopping" && p.j.Loss == "") {
				t.Fatalf("unsafe transition after %s: %+v", fault, p.j.Operation)
			}
		})
	}
}
func TestPersistentMinimumAndAccessModeBoundary(t *testing.T) {
	if !slices.Equal(persistentAccessModes(Options{LauncherImage: "pin"}), []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}) {
		t.Fatal("production not RWOP")
	}
	p := persistentSetup(t)
	p.j.Operation.From = 2
	p.j.Operation.To = 1
	p.j.Applied = 2
	if e := p.r.saveJournal(t.Context(), p.res, p.j); e != nil {
		t.Fatal(e)
	}
	p.step(t)
	if p.j.Operation.Phase != "Blocked" {
		t.Fatal("last follower retired without watermark")
	}
}

func TestPersistentLostStopAndReplicaResponsesReplayOnce(t *testing.T) {
	p := persistentSetup(t)
	original := p.r.launcherCall
	stops := 0
	p.r.launcherCall = func(ctx context.Context, f *fleet.CelldFleet, pod *corev1.Pod, op, gen string) (launcher.State, error) {
		state, err := original(ctx, f, pod, op, gen)
		if op != "" {
			stops++
			if stops == 1 {
				return launcher.State{}, errors.New("response lost after child stopped")
			}
		}
		return state, err
	}
	p.step(t)
	p.step(t)
	p.reader.barrier = true
	p.step(t)
	if p.j.Operation.Phase != "Stopping" || stops != 1 {
		t.Fatal("lost stop response not retained")
	}
	p.step(t)
	if p.j.Operation.Phase != "Retiring" || stops != 1 {
		t.Fatal("graceful stop replay issued a second command")
	}
	if err := p.r.applyReplicas(t.Context(), p.w, p.j.Operation); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit persisting Recovering, then reload the durable journal.
	p.step(t)
	if replicas(p.w) != 2 || p.j.Operation.Phase != "Recovering" {
		t.Fatal("replica response loss caused another decrement")
	}
}
func TestPersistentReactivationRejectsChangedPVBeforeScheduling(t *testing.T) {
	p := persistentSetup(t)
	p.step(t)
	p.step(t)
	p.reader.barrier = true
	p.step(t)
	p.step(t)
	pod := &corev1.Pod{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, pod); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	p.step(t)
	p.reader.now = p.reader.now.Add(11 * time.Second)
	p.step(t)
	pod.UID = "successor"
	pod.ResourceVersion = ""
	pod.Spec.NodeName = ""
	pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: launcherGate}}
	if err := p.r.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Name: "pv-persistent-2"}, pv); err != nil {
		t.Fatal(err)
	}
	pv.Spec.HostPath.Path = "/other-disk"
	if err := p.r.Update(t.Context(), pv); err != nil {
		t.Fatal(err)
	}
	if err := p.r.schedulePersistent(t.Context(), p.f, p.j); err == nil {
		t.Fatal("changed physical volume admitted")
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.SchedulingGates) != 1 {
		t.Fatal("unsafe successor released to scheduler")
	}
}
func TestPersistentJournalRejectsIncompleteStopAuthority(t *testing.T) {
	p := persistentSetup(t)
	p.step(t)
	p.j.PersistentHistory = append(p.j.PersistentHistory, p.j.Operation.PersistentMembers[0])
	p.j.PersistentHistory[0].Retired = true
	if err := validatePersistentJournal(p.j); err == nil {
		t.Fatal("retired without positive stop accepted")
	}
	p.j.PersistentHistory[0].Stopped = true
	p.j.PersistentHistory[0].VolumeUID = ""
	if err := validatePersistentJournal(p.j); err == nil {
		t.Fatal("missing volume binding accepted")
	}
}

func TestPersistentLauncherCredentialCreationReplay(t *testing.T) {
	p := persistentSetup(t)
	if err := p.r.createLauncherKey(t.Context(), p.f, p.res); err != nil {
		t.Fatal(err)
	}
	first, err := p.r.launcherKey(t.Context(), p.f)
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after immutable Secret creation and before digest persistence.
	delete(p.res.Annotations, launcherKeyDigest)
	if err := p.r.Update(t.Context(), p.res); err != nil {
		t.Fatal(err)
	}
	if err := p.r.createLauncherKey(t.Context(), p.f, p.res); err != nil {
		t.Fatal(err)
	}
	replayed, err := p.r.launcherKey(t.Context(), p.f)
	if err != nil || !slices.Equal(first, replayed) {
		t.Fatal("credential creation changed during replay", err)
	}
	// A foreign provisioning nonce never authorizes adopting the existing key.
	delete(p.res.Annotations, launcherKeyDigest)
	p.res.Annotations["celld.example.com/launcher-creation"] = "foreign"
	if err := p.r.Update(t.Context(), p.res); err != nil {
		t.Fatal(err)
	}
	if err := p.r.createLauncherKey(t.Context(), p.f, p.res); err == nil {
		t.Fatal("adopted unrelated credential")
	}
}

func TestPersistentPauseCannotCompleteReactivation(t *testing.T) {
	for _, phase := range []string{"Reactivating", "Prepared"} {
		t.Run(phase, func(t *testing.T) {
			p := persistentSetup(t)
			p.res.Annotations[attemptAnnotation] = "created"
			for name := range p.j.Claims {
				claim := &corev1.PersistentVolumeClaim{}
				if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: name}, claim); err != nil {
					t.Fatal(err)
				}
				claim.Labels = labels(p.f)
				claim.Annotations = map[string]string{"celld.example.com/storage-reservation": p.res.Name}
				if err := p.r.Update(t.Context(), claim); err != nil {
					t.Fatal(err)
				}
			}

			members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
			if err != nil {
				t.Fatal(err)
			}
			donor := members[2]
			donor.Stopped, donor.Retired = true, true
			p.j.PersistentHistory = []persistentMember{donor}
			p.j.Applied = 2
			p.j.Operation = &lifecycleOperation{ID: "reuse", Phase: phase, From: 2, To: 3, StartedAt: p.reader.now, Deadline: p.reader.now.Add(operationBudget)}
			p.w.Annotations = map[string]string{operationKey: "reuse"}
			if err := p.r.Update(t.Context(), p.w); err != nil {
				t.Fatal(err)
			}
			if err := p.r.saveJournal(t.Context(), p.res, p.j); err != nil {
				t.Fatal(err)
			}
			p.f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
			if _, _, err := p.r.lifecycle(t.Context(), p.f, p.res, p.w); err != nil {
				t.Fatal(err)
			}
			if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.res), p.res); err != nil {
				t.Fatal(err)
			}
			j, err := readJournal(p.res)
			if err != nil {
				t.Fatal(err)
			}
			if j.Applied != 2 || j.Operation == nil {
				t.Fatal("pause certified incomplete reactivation after replica issuance")
			}
		})
	}
}
