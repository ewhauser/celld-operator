package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v041 "github.com/ewhauser/celld-operator/internal/runtime/v041"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type coordinatedReader struct {
	*persistentReader
	sealed bool
}

func (r *coordinatedReader) List(ctx context.Context, prefix, cursor string) (v050.Page, error) {
	p, e := r.persistentReader.List(ctx, prefix, cursor)
	p.Keys = slices.DeleteFunc(p.Keys, func(k string) bool { return k == "nodes/persistent-2.json" })
	return p, e
}
func (r *coordinatedReader) Get(ctx context.Context, key string) ([]byte, error) {
	b, e := r.persistentReader.Get(ctx, key)
	if e != nil {
		return nil, e
	}
	var node map[string]any
	if e := json.Unmarshal(b, &node); e != nil {
		return nil, e
	}
	name := node["node"].(string)
	if r.sealed && r.generation[name] != "new-"+name {
		node["expires_ms"] = r.now.Add(-time.Second).UnixMilli()
		node["log"].(map[string]any)["state"] = "sealed"
	}
	return json.Marshal(node)
}

func coordinatedSetup(t *testing.T) (*persistentFixture, *coordinatedReader) {
	t.Helper()
	p := persistentSetup(t)
	pod := &corev1.Pod{}
	if e := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, pod); e != nil {
		t.Fatal(e)
	}
	if e := p.r.Delete(t.Context(), pod); e != nil {
		t.Fatal(e)
	}
	p.j.Applied = 2
	p.j.Operation = nil
	p.j.Inventory.Sessions = p.j.Inventory.Sessions[:2]
	setReplicas(p.w, 2)
	if e := p.r.Update(t.Context(), p.w); e != nil {
		t.Fatal(e)
	}
	p.f.Spec.Maintenance = &fleet.MaintenanceSpec{AllowCoordinatedDowntime: true}
	if e := p.r.Update(t.Context(), p.f); e != nil {
		t.Fatal(e)
	}
	reader := &coordinatedReader{persistentReader: p.reader}
	p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return reader, nil }
	return p, reader
}

func TestCoordinatedSmallFleetRestartAndContraction(t *testing.T) {
	for _, kind := range []string{"Restart", "Contract", "Upgrade"} {
		t.Run(kind, func(t *testing.T) {
			p, reader := coordinatedSetup(t)
			target := int32(2)
			if kind == "Contract" {
				target = 1
			}
			p.f.Spec.Replicas = target
			p.f.Spec.Maintenance.RestartToken = "restart-one"
			if e := p.r.Update(t.Context(), p.f); e != nil {
				t.Fatal(e)
			}
			p.j.Maintenance = &maintenanceOperation{ID: "coordinated", Kind: kind, Token: "restart-one", Phase: "Capture", Coordinated: true, TargetReplicas: target, StartedAt: reader.now, Deadline: reader.now.Add(time.Minute)}
			if kind == "Upgrade" {
				p.j.RuntimeImage = v041.Image
				p.j.Maintenance.SourceImage, p.j.Maintenance.TargetImage = v041.Image, Image
				source := p.f.DeepCopy()
				source.Spec.RuntimeImage = v041.Image
				p.w.Spec.Template.Spec.Containers[0] = workload(source, p.r.Options).(*appsv1.StatefulSet).Spec.Template.Spec.Containers[0]
				if e := p.r.Update(t.Context(), p.w); e != nil {
					t.Fatal(e)
				}
				for i := range 2 {
					pod := &corev1.Pod{}
					if e := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: fmt.Sprintf("persistent-%d", i)}, pod); e != nil {
						t.Fatal(e)
					}
					pod.Spec.Containers[0] = p.w.Spec.Template.Spec.Containers[0]
					if e := p.r.Update(t.Context(), pod); e != nil {
						t.Fatal(e)
					}
					id, _ := podIdentity(pod)
					for k := range p.j.Inventory.Sessions {
						if p.j.Inventory.Sessions[k].Node == pod.Name {
							p.j.Inventory.Sessions[k].Container = id
						}
					}
				}
			}
			if e := p.r.saveJournal(t.Context(), p.res, p.j); e != nil {
				t.Fatal(e)
			}
			step := func() {
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
				if _, _, e = p.r.executeMaintenance(t.Context(), p.f, p.res, p.j, p.w); e != nil {
					t.Fatal(e)
				}
			}
			step() // capture exact original invocations
			for range 3 {
				step()
			} // stop every child, then refuse unsealed logs
			if p.j.Maintenance.Phase != "Stopping" || replicas(p.w) != 2 {
				t.Fatal("unsealed shutdown moved compute")
			}
			for _, member := range p.j.Maintenance.Persistent {
				if !member.Stopped || !member.RestartDenied {
					t.Fatal("stop not durable")
				}
			}
			reader.sealed = true
			step() // stop => Quiesced
			if p.j.Maintenance.Phase != "Quiesced" {
				t.Fatal(p.j.Maintenance.Phase)
			}
			// Pause after physical stops cannot strand already admitted recovery.
			p.f.Spec.Maintenance.Paused = true
			if e := p.r.Update(t.Context(), p.f); e != nil {
				t.Fatal(e)
			}
			step() // preserve retirement authority => Empty
			step() // scale zero
			if replicas(p.w) != 0 {
				t.Fatal("no zero boundary", p.j.Maintenance.Phase)
			}
			old := []*corev1.Pod{}
			for i := range 2 {
				pod := &corev1.Pod{}
				if e := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: fmt.Sprintf("persistent-%d", i)}, pod); e != nil {
					t.Fatal(e)
				}
				old = append(old, pod.DeepCopy())
				if e := p.r.Delete(t.Context(), pod); e != nil {
					t.Fatal(e)
				}
			}
			step() // empty evidence, persist Resuming
			step() // restore target
			if replicas(p.w) != target {
				t.Fatal("target not restored", p.j.Maintenance.Phase)
			}
			for i := range target {
				pod := old[i]
				pod.ResourceVersion = ""
				if kind == "Upgrade" {
					pod.Spec.Containers[0] = p.w.Spec.Template.Spec.Containers[0]
				}
				pod.UID = types.UID("new-" + pod.Name)
				pod.Status.ContainerStatuses[0].ContainerID = "new-container"
				if e := p.r.Create(t.Context(), pod); e != nil {
					t.Fatal(e)
				}
				state := p.states[pod.Name]
				state.PodUID = string(pod.UID)
				state.Invocation = "new-" + pod.Name
				state.Generation = "new-" + pod.Name
				state.Phase = "Running"
				state.Operation = ""
				state.RestartDenied = false
				p.states[pod.Name] = state
				reader.generation[pod.Name] = state.Generation
			}
			step() // settle
			if p.j.Maintenance == nil || p.j.Maintenance.SettledAt.IsZero() {
				t.Fatal("successors not admitted")
			}
			reader.now = reader.now.Add(11 * time.Second)
			step()
			if p.j.Maintenance != nil || p.j.Applied != target {
				t.Fatal("coordinated operation incomplete")
			}
			if kind == "Upgrade" && p.j.RuntimeImage != Image {
				t.Fatal("completed upgrade did not promote applied image")
			}
			if kind == "Restart" && !slices.Contains(p.j.CompletedRestarts, "restart-one") {
				t.Fatal("token not completed")
			}
			for _, pod := range old {
				claim := &corev1.PersistentVolumeClaim{}
				if e := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "data-" + pod.Name}, claim); e != nil {
					t.Fatal("retained PVC removed", e)
				}
			}
		})
	}
}

func TestCoordinatedContractionRequiresOptInAndAZFloor(t *testing.T) {
	for _, scenario := range []string{"default", "multiple-az", "automatic", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := coordinatedSetup(t)
			p.f.Spec.Replicas = 1
			p.j.Operation = &lifecycleOperation{ID: "shrink", From: 2, To: 1, Phase: "Blocked", Deadline: p.reader.now.Add(time.Minute)}
			switch scenario {
			case "default":
				p.f.Spec.Maintenance.AllowCoordinatedDowntime = false
			case "multiple-az":
				p.f.Spec.Placement.AZCount = 2
			case "automatic":
				p.j.Operation.Automatic = true
			case "paused":
				p.f.Spec.Maintenance.Paused = true
			}
			if _, _, e := p.r.beginCoordinatedContraction(t.Context(), p.f, p.res, p.j, p.w); e != nil {
				t.Fatal(e)
			}
			if p.j.Maintenance != nil || p.j.Operation == nil || replicas(p.w) != 2 {
				t.Fatal("unsafe coordinated contraction admitted")
			}
		})
	}
}

func TestCoordinatedZeroRequiresFreshExactStopAndSealedEvidence(t *testing.T) {
	for _, fault := range []string{"resurrection", "invocation", "loss", "unsealed", "disk"} {
		t.Run(fault, func(t *testing.T) {
			p, reader := coordinatedSetup(t)
			view := *p.j
			view.Operation = &lifecycleOperation{ID: "coordinated"}
			members, e := p.r.persistentMembers(t.Context(), p.f, &view, 2, false)
			if e != nil {
				t.Fatal(e)
			}
			for i := range members {
				members[i].Stopped = true
				members[i].RestartDenied = true
				members[i].Epoch = 1
				state := p.states[members[i].Node]
				state.Phase = "Stopped"
				state.RestartDenied = true
				state.Operation = "coordinated"
				p.states[members[i].Node] = state
			}
			p.j.Maintenance = &maintenanceOperation{ID: "coordinated", Kind: "Contract", Phase: "Empty", Coordinated: true, TargetReplicas: 1, Persistent: members}
			reader.sealed = true
			switch fault {
			case "resurrection":
				state := p.states["persistent-0"]
				state.RestartDenied = false
				p.states["persistent-0"] = state
			case "invocation":
				state := p.states["persistent-0"]
				state.Invocation = "other"
				p.states["persistent-0"] = state
			case "loss":
				reader.loss = true
			case "unsealed":
				reader.sealed = false
			case "disk": // A changed mounted disk must not inherit an old stop receipt.
				state := p.states["persistent-0"]
				state.DiskID = "different"
				p.states["persistent-0"] = state
			}
			var blocked error
			_, _, e = p.r.executeCoordinatedPersistent(t.Context(), p.f, p.res, p.j, p.w, func(err error) (ctrl.Result, bool, error) { blocked = err; return ctrl.Result{}, true, nil })
			if e != nil {
				t.Fatal(e)
			}
			if blocked == nil || replicas(p.w) != 2 {
				t.Fatal("unsafe zero-boundary accepted", fault)
			}
		})
	}
}
