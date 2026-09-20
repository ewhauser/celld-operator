package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v041 "github.com/ewhauser/celld-operator/internal/runtime/v041"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type maintenanceReader struct {
	v050.Reader
	allSealed bool
	noLog     bool
}

func (m maintenanceReader) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := m.Reader.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	node := strings.TrimSuffix(strings.TrimPrefix(key, "nodes/"), ".json")
	if suffix, ok := strings.CutPrefix(node, "pod-"); ok {
		data["addr"] = "10.0.0." + suffix + ":8081"
	}
	if m.allSealed {
		data["expires_ms"] = 1
		if m.noLog {
			delete(data, "log")
		} else {
			data["log"].(map[string]any)["state"] = "sealed"
		}
	}
	return json.Marshal(data)
}

func TestBucketRestartExactUIDRecoveryAndTokenReplay(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	p.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return maintenanceReader{Reader: reader}, nil
	}
	f.Spec.Replicas = 3
	f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "one"}
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	j.Version = 6
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Operation = nil
	j.Maintenance = &maintenanceOperation{ID: "restart", Kind: "Restart", Token: "one", Phase: "Capture", Deadline: reader.now.Add(time.Hour)}
	w := workload(f, opts).(*appsv1.Deployment)
	w.UID = j.WorkloadUID
	res := &fleet.CelldStorageReservation{Name: reservationName(f)}
	if err := p.client.Create(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	pods := &corev1.PodList{}
	if err := p.client.List(t.Context(), pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		pod.Status.PodIP = "10.0.0." + strings.TrimPrefix(pod.Name, "pod-")
		if err := p.client.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: p.client, Evidence: p, Options: opts, now: p.now}
	step := func() {
		t.Helper()
		o := capacity.Observation{At: reader.now, Complete: true}
		current := &corev1.PodList{}
		if err := r.List(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		for i := range current.Items {
			id, _ := podIdentity(&current.Items[i])
			o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
		}
		r.Collector = bucketCapacityCollector{o}
		if _, _, err := r.executeMaintenance(t.Context(), f, res, j, w); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if j.Maintenance.Phase != "Authorized" {
		t.Fatalf("not admitted: %+v", j.Maintenance)
	}
	captured := j.Maintenance.Targets[0]
	original := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: captured.Name}, original); err != nil {
		t.Fatal(err)
	}
	// Even durable admission cannot reuse stale health/capacity evidence to delete.
	priorDeadline := j.Maintenance.Deadline
	j.Maintenance.Deadline = reader.now.Add(-time.Second)
	step()
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(original), &corev1.Pod{}); err != nil {
		t.Fatal("expired admission deleted target", err)
	}
	j.Maintenance.Deadline = priorDeadline
	originalReady := original.DeepCopy()
	original.Status.Conditions = nil
	if err := r.Status().Update(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	step()
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(original), original); err != nil {
		t.Fatal("unready target deleted", err)
	}
	original.Status = originalReady.Status
	if err := r.Status().Update(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	// A same-UID container successor must be admitted durably before deletion.
	reader.generation = map[string]string{original.Name: "successor-generation"}
	original.Status.ContainerStatuses[0].ContainerID = "successor-container"
	if err := r.Status().Update(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	step()
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(original), &corev1.Pod{}); err != nil {
		t.Fatal("successor deleted before durable admission", err)
	}
	if !slices.ContainsFunc(j.Maintenance.Sessions, func(s bucketSession) bool {
		return s.Node == string(original.UID) && s.Generation == "successor-generation"
	}) {
		t.Fatal("successor generation not persisted before deletion")
	}
	// Pause after authorization cannot discard a previous leader's delete authority.
	f.Spec.Maintenance.Paused = true
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	step()
	if j.Maintenance.Phase != "Recovering" {
		t.Fatal("authorized action lost on pause")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(original), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("target not deleted")
	}
	// Replay same UID deletion cannot delete a new object with the same name.
	replacement := original.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "pod-9"
	replacement.Status.PodIP = "10.0.0.9"
	if err := r.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	reader.nodes = append(reader.nodes, "pod-9")
	j.Maintenance.Phase = "Authorized"
	step()
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(replacement), &corev1.Pod{}); err != nil {
		t.Fatal("replacement deleted by old authority", err)
	}
	step()
	if !j.Maintenance.SettledAt.IsZero() {
		t.Fatal("renewing old process accepted")
	}
	reader.expired = map[string]bool{string(captured.UID): true}
	step()
	reader.now = reader.now.Add(11 * time.Second)
	step()
	if j.Maintenance.Index != 1 || j.Maintenance.Phase != "Next" {
		t.Fatalf("recovery not completed %+v", j.Maintenance)
	}
	step()
	if j.Maintenance.Phase != "Next" {
		t.Fatal("pause admitted next target")
	}
	f.Spec.Maintenance.Paused = false
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	step()
	if w.GetAnnotations()[maintenanceFenceKey] != "" || j.Maintenance.Phase != "Authorized" {
		t.Fatalf("unpause left stale workload fence or failed next admission: %+v", j.Maintenance)
	}
	j.CompletedRestarts = []string{"one"}
	j.Maintenance = nil
	j.Request = nil
	if kind, _, err := r.persistDisruptionRequest(t.Context(), f, res, j, w); err != nil || kind != "" {
		t.Fatal("completed token replayed", kind, err)
	}
}

func TestPersistentShutdownRequiresEverySealedLeader(t *testing.T) {
	p := persistentSetup(t)
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, nil, true); err == nil {
		t.Fatal("empty shutdown capture accepted")
	}
	p.j.Operation = nil
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range members {
		members[i].Stopped = true
		members[i].RestartDenied = true
		members[i].Epoch = 1
	}
	p.reader.stopped = true
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("one sealed donor accepted as whole fleet durability")
	}
	p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return maintenanceReader{Reader: p.reader, allSealed: true}, nil
	}
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err != nil {
		t.Fatal(err)
	}
	p.reader.loss = true
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("historical loss ignored")
	}
	p.reader.loss = false
	p.reader.missing = true
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("missing stopped leader treated as sealed")
	}
}

func TestDeletionBeforeProvisioningLeavesPermanentReservation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			f := fixture("fresh", "fresh-data", "Bucket")
			f.Finalizers = []string{Finalizer}
			r := setup(t, f)
			if existing {
				res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{InitialReplicas: f.Spec.Replicas, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
				if err := r.Create(t.Context(), res); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Delete(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				latest := &fleet.CelldFleet{}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), latest); apierrors.IsNotFound(err) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if _, err := r.deleteFleet(t.Context(), latest); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
				t.Fatal("never-provisioned fleet not finalized")
			}
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			if res.Annotations[attemptAnnotation] != "deletion-before-workload" {
				t.Fatal("late creation not fenced")
			}
		})
	}
}

func TestDeletionBeforeProvisioningResumesAfterConflictOnLegacyReservation(t *testing.T) {
	f := fixture("legacy", "legacy-data", "Bucket")
	f.Spec.RuntimeImage = v041.Image
	f.Finalizers = []string{Finalizer}
	// Reservations created before InitialReplicas existed leave the field unset.
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
	r := setup(t, f, res)
	if err := r.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	once := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*fleet.CelldFleet); ok && !once {
			// The finalizer patch carries an optimistic lock; a concurrent writer
			// invalidates it exactly once.
			once = true
			other := &fleet.CelldFleet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), other); err != nil {
				return err
			}
			other.Labels = map[string]string{"example.com/touched": "true"}
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.Patch(ctx, obj, p, opts...)
	}})
	step := func() error {
		t.Helper()
		latest := &fleet.CelldFleet{}
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), latest); err != nil {
			return err
		}
		_, err := r.deleteFleet(t.Context(), latest)
		return err
	}
	if err := step(); !apierrors.IsConflict(err) {
		t.Fatalf("expected the finalizer patch to conflict, got %v", err)
	}
	if !once {
		t.Fatal("conflict never injected")
	}
	stored := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, stored); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(stored)
	if err != nil {
		t.Fatalf("legacy deletion journal rejected on reload: %v", err)
	}
	if j.Initial != f.Spec.Replicas || j.Applied != f.Spec.Replicas || j.RuntimeImage != v041.Image {
		t.Fatalf("legacy journal baseline %d/%d image %q", j.Initial, j.Applied, j.RuntimeImage)
	}
	if err := step(); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deletion never completed after the conflict: %v", err)
	}
}

func TestCleanupRequiresExactAuthority(t *testing.T) {
	f := fixture("cleanup", "cleanup-data", "Bucket")
	f.Finalizers = []string{Finalizer}
	r := setup(t, f)
	f.DeletionTimestamp = new(metav1.Now())
	if _, _, err := r.completeRetainedDeletion(t.Context(), f, &fleet.CelldStorageReservation{}, &lifecycleJournal{}); err == nil {
		t.Fatal("cleanup without durable shutdown admitted")
	}
}

func TestBucketRetainDataShutdownCompletesOnlyAfterAllLeasesExpire(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	f.Spec.Replicas = 3
	f.Finalizers = []string{Finalizer}
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	j.Version = 6
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Operation = nil
	j.Maintenance = &maintenanceOperation{ID: "delete", Kind: "Delete", Phase: "Capture", Deadline: reader.now.Add(time.Hour)}
	w := workload(f, opts).(*appsv1.Deployment)
	w.UID = j.WorkloadUID
	w.Annotations = map[string]string{maintenanceFenceKey: "deleting"}
	res := &fleet.CelldStorageReservation{Name: reservationName(f)}
	if err := p.client.Create(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Create(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Options: opts, Evidence: p, now: p.now}
	o := capacity.Observation{At: reader.now, Complete: true}
	pods := &corev1.PodList{}
	if err := r.List(t.Context(), pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		id, _ := podIdentity(&pods.Items[i])
		o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
	}
	r.Collector = bucketCapacityCollector{o}
	var blocked error
	step := func() {
		t.Helper()
		blocked = nil
		_, _, err := r.executeBucketDeletion(t.Context(), f, res, j, w, func(e error) (ctrl.Result, bool, error) { blocked = e; return ctrl.Result{}, true, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	step()
	if j.Maintenance.Phase != "Authorized" {
		t.Fatal("shutdown not captured", blocked)
	}
	step()
	if replicas(w) != 0 || j.Maintenance.Phase != "Recovering" {
		t.Fatal("shutdown replicas not issued")
	}
	for i := range pods.Items {
		if err := r.Delete(t.Context(), &pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if blocked == nil {
		t.Fatal("live old leases allowed completion")
	}
	reader.expired = map[string]bool{"pod-0": true, "pod-1": true, "pod-2": true}
	step()
	if blocked != nil {
		t.Fatal(blocked)
	}
	reader.now = reader.now.Add(11 * time.Second)
	step()
	step()
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
		t.Fatal("fleet not finalized", err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), &fleet.CelldStorageReservation{}); err != nil {
		t.Fatal("permanent reservation removed", err)
	}
}

func TestPersistentRestartWaitsForFollowerBarrierAndReusesExactDisk(t *testing.T) {
	p := persistentSetup(t)
	p.j.Operation = nil
	p.f.Spec.Replicas = 3
	p.j.Maintenance = &maintenanceOperation{ID: "restart-pf", Kind: "Restart", Token: "one", Phase: "Next", Deadline: p.reader.now.Add(time.Hour), Targets: []maintenanceTarget{{Name: "persistent-2", UID: "uid-persistent-2"}}}
	var blocked error
	step := func() {
		t.Helper()
		blocked = nil
		pods := &corev1.PodList{}
		if err := p.r.List(t.Context(), pods); err != nil {
			t.Fatal(err)
		}
		o := capacity.Observation{At: p.reader.now, Complete: true}
		for i := range pods.Items {
			id, _ := podIdentity(&pods.Items[i])
			o.Samples = append(o.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: p.reader.now, RuntimeReceived: p.reader.now, MetricsAt: p.reader.now, MetricsReceived: p.reader.now, Window: 15 * time.Second})
		}
		p.r.Collector = bucketCapacityCollector{o}
		_, _, err := p.r.executePersistentMaintenance(t.Context(), p.f, p.res, p.j, p.w, func(err error) (ctrl.Result, bool, error) { blocked = err; return ctrl.Result{}, true, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	step()
	if p.j.Maintenance.Phase != "Stopping" {
		t.Fatal("not admitted", blocked)
	}
	step()
	if blocked == nil || p.j.Maintenance.Phase != "Stopping" {
		t.Fatal("follower barrier bypassed")
	}
	p.reader.barrier = true
	step()
	if p.j.Maintenance.Phase != "Authorized" {
		t.Fatal("qualified donor not authorized", blocked)
	}
	pod := &corev1.Pod{}
	if err := p.r.Get(t.Context(), client.ObjectKey{Namespace: p.f.Namespace, Name: "persistent-2"}, pod); err != nil {
		t.Fatal(err)
	}
	step()
	if p.j.Maintenance.Phase != "Recovering" {
		t.Fatal("not awaiting replacement")
	}
	pod.ResourceVersion = ""
	pod.UID = "replacement"
	pod.Status.ContainerStatuses[0].ContainerID = "replacement-container"
	if err := p.r.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	state := p.states[pod.Name]
	state.PodUID = string(pod.UID)
	state.Invocation = "replacement-invocation"
	state.Generation = "replacement-generation"
	state.Phase = "Running"
	state.Operation = ""
	p.states[pod.Name] = state
	p.reader.generation[pod.Name] = state.Generation
	p.reader.stopped = false
	step()
	if blocked != nil {
		t.Fatal(blocked)
	}
	p.reader.now = p.reader.now.Add(11 * time.Second)
	step()
	if p.j.Maintenance.Phase != "Next" || p.j.Maintenance.Index != 1 {
		t.Fatal("replacement did not settle", blocked)
	}
	step()
	if p.j.Maintenance != nil || !slices.Contains(p.j.CompletedRestarts, "one") {
		t.Fatal("restart token not completed")
	}
}

func TestRestartAdmissionLosesToAcknowledgedPauseFence(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	stale := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), stale); err != nil {
		t.Fatal(err)
	}
	current := stale.DeepCopyObject().(client.Object)
	if current.GetAnnotations() == nil {
		current.SetAnnotations(map[string]string{})
	}
	current.GetAnnotations()[maintenanceFenceKey] = "paused"
	if err := r.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := r.authorizeMaintenanceAction(t.Context(), stale, &maintenanceOperation{ID: "late", Index: 0}); err == nil {
		t.Fatal("delayed restart admission bypassed acknowledged pause")
	}
}

func TestAllStoppedNoOwnLogRequiresNoPositiveHistoricalEpoch(t *testing.T) {
	p := persistentSetup(t)
	p.j.Operation = nil
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range members {
		members[i].Stopped = true
		members[i].RestartDenied = true
		members[i].Epoch = 0
	}
	for i := range p.j.Inventory.Sessions {
		p.j.Inventory.Sessions[i].Epoch = 0
	}
	p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return maintenanceReader{Reader: p.reader, allSealed: true, noLog: true}, nil
	}
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err != nil {
		t.Fatal(err)
	}
	p.j.Inventory.Sessions[0].Epoch = 1
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("lost historical own log accepted")
	}
	p.j.Inventory.Sessions[0].Epoch = 0
	p.j.PersistentHistory = append(p.j.PersistentHistory, members[0])
	p.j.PersistentHistory[0].Epoch = 1
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("retained historical own log accepted as absent")
	}
}

func TestPersistentRestartRevalidatesSurvivorsBeforeStop(t *testing.T) {
	for _, failure := range []string{"expired", "survivorChanged"} {
		t.Run(failure, func(t *testing.T) {
			p := persistentSetup(t)
			p.j.Operation = nil
			p.f.Spec.Replicas = 3
			members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
			if err != nil {
				t.Fatal(err)
			}
			p.j.Maintenance = &maintenanceOperation{ID: "restart", Kind: "Restart", Token: "token", Phase: "Stopping", Deadline: p.reader.now.Add(time.Minute), Targets: []maintenanceTarget{{Name: "persistent-2", UID: "uid-persistent-2"}}, Persistent: members}
			if failure == "expired" {
				p.j.Maintenance.Deadline = p.reader.now.Add(-time.Second)
			} else {
				s := p.states["persistent-0"]
				s.Invocation = "unadmitted-survivor"
				p.states["persistent-0"] = s
			}
			var blocked error
			if _, _, err := p.r.executePersistentMaintenance(t.Context(), p.f, p.res, p.j, p.w, func(err error) (ctrl.Result, bool, error) { blocked = err; return ctrl.Result{}, true, nil }); err != nil {
				t.Fatal(err)
			}
			if blocked == nil || p.states["persistent-2"].Phase != "Running" {
				t.Fatal("unsafe donor stop issued", blocked)
			}
		})
	}
}

func TestShutdownRejectsLegacyStoppedReceiptWithoutResurrectionDenial(t *testing.T) {
	p := persistentSetup(t)
	p.j.Operation = nil
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &maintenanceOperation{ID: "shutdown", Kind: "Delete", Phase: "Stopping", Persistent: members}
	for i := range m.Persistent {
		member := &m.Persistent[i]
		member.Stopped = true
		member.RestartDenied = true
		state := p.states[member.Node]
		state.Phase = "Stopped"
		state.Operation = m.ID
		state.RestartDenied = true
		p.states[member.Node] = state
	}
	if err := p.r.verifyShutdownStops(t.Context(), p.f, m); err != nil {
		t.Fatal(err)
	}
	state := p.states[m.Persistent[0].Node]
	state.RestartDenied = false
	p.states[m.Persistent[0].Node] = state
	if err := p.r.verifyShutdownStops(t.Context(), p.f, m); err == nil {
		t.Fatal("legacy launcher stop receipt accepted")
	}
	state.RestartDenied = true
	p.states[m.Persistent[0].Node] = state
	m.Persistent[0].RestartDenied = false
	if err := p.r.verifyShutdownStops(t.Context(), p.f, m); err == nil {
		t.Fatal("legacy captured authority upgraded silently")
	}
}

func TestOrderedRestartUsesActualTargetForAZSafety(t *testing.T) {
	f := fixture("ordered", "ordered-data", "Bucket")
	f.Spec.BucketWorkload = "Ordered"
	candidates := map[types.UID]bucketCandidate{
		"a0": {Hostname: "a0", Zone: "us-east-1a"},
		"b1": {Hostname: "b1", Zone: "us-east-1b"},
		"a2": {Hostname: "a2", Zone: "us-east-1a"},
	}
	if err := validateRestartPlacement(f, candidates, "a0"); err != nil {
		t.Fatal("safe arbitrary A target rejected", err)
	}
	if err := validateRestartPlacement(f, candidates, "a2"); err != nil {
		t.Fatal("safe highest A target rejected", err)
	}
	if err := validateRestartPlacement(f, candidates, "b1"); err == nil {
		t.Fatal("sole B target removal violates strict placement")
	}
	if len(candidates) != 3 {
		t.Fatal("placement admission mutated shared evidence")
	}
}
