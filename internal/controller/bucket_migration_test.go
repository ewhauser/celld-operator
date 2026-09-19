package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBucketMigrationRequiresPositiveExpiryAndRetainsHistory(t *testing.T) {
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	p.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return maintenanceReader{Reader: reader}, nil
	}
	f.Spec.Replicas = 3
	j.Version = 7
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Operation = nil
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{InitialReplicas: 3, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
	w := workload(f, opts).(*appsv1.Deployment)
	w.UID = j.WorkloadUID
	for _, o := range []client.Object{res, w} {
		if err := p.client.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	pods := &corev1.PodList{}
	if err := p.client.List(t.Context(), pods, client.InNamespace(f.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pods.Items[i].Status.PodIP = "10.0.0." + string(rune('0'+i))
		if err := p.client.Status().Update(t.Context(), &pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	f.Spec.BucketWorkload = "Ordered"
	f.Spec.Maintenance = &fleet.MaintenanceSpec{AllowCoordinatedDowntime: true, OrderedMigrationToken: "ordered-1"}
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Options: opts, Evidence: p, now: func() time.Time { return reader.now }}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}
	step := func() {
		t.Helper()
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
			t.Fatal(err)
		}
		_, handled, err := r.migrateBucket(t.Context(), f, res)
		if err != nil || !handled {
			t.Fatalf("migration handled=%v err=%v", handled, err)
		}
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(res), res); err != nil {
			t.Fatal(err)
		}
		j, err = r.loadJournal(t.Context(), res)
		if err != nil {
			t.Fatal(err)
		}
	}
	step()
	step()
	if j.BucketMigration.Phase != "Authorized" {
		t.Fatalf("capture failed: %+v", j.BucketMigration)
	}
	step()
	if j.BucketMigration.Phase != "Recovering" {
		t.Fatalf("shutdown failed: %+v", j.BucketMigration)
	}
	for i := range pods.Items {
		if err := r.Delete(t.Context(), &pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if j.BucketMigration.Phase != "Recovering" || !j.BucketMigration.SettledAt.IsZero() {
		t.Fatal("unexpired writer resolved")
	}
	reader.expired = map[string]bool{"pod-0": true, "pod-1": true, "pod-2": true}
	step()
	reader.now = reader.now.Add(11 * time.Second)
	step()
	if j.BucketMigration.Phase != "DeleteOld" {
		t.Fatalf("expiry not accepted: %+v", j.BucketMigration)
	}
	// Simulate Kubernetes foreground GC finishing the source workload.
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(w), w); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	step()
	if j.BucketMigration.Phase != "CreateNew" {
		t.Fatal("missing creation intent")
	}
	// Persist creation authority before any API create may be issued.
	step()
	if !j.BucketMigration.CreationAuthorized {
		t.Fatal("missing durable create authority")
	}
	// API server assigns UIDs; fake client does not. A replayed created object
	// carries the exact retained operation ID and template.
	target := workload(f, opts).(*appsv1.StatefulSet)
	setReplicas(target, 0)
	target.UID = "ordered-uid"
	target.Annotations = map[string]string{migrationKey: j.BucketMigration.ID}
	if err := r.Create(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	step()
	step()
	if j.BucketMigration.Phase != "Complete" || j.WorkloadUID != target.UID || len(j.BucketHistory) != 3 {
		t.Fatalf("migration lost authority: %+v", j)
	}
	for _, s := range j.BucketHistory {
		if !s.Retired || !s.ExpiryObserved {
			t.Fatal("writer history not retained")
		}
	}
	want := res.Spec
	if !r.reservationMatches(t.Context(), f, res, want) {
		t.Fatal("original reservation binding lost")
	}
}

func TestBucketMigrationRejectsOverlapsAndReplacedTarget(t *testing.T) {
	for _, phase := range []string{"Capture", "Authorized", "Recovering", "DeleteOld", "CreateNew"} {
		j := &lifecycleJournal{WorkloadUID: "old", BucketMigration: &bucketMigration{ID: "id", Token: "token", Phase: phase, SourceUID: "old"}, Operation: &lifecycleOperation{ID: "overlap"}}
		if validateBucketMigration(j) == nil {
			t.Fatalf("accepted overlapping %s", phase)
		}
		j.Operation = nil
		j.WorkloadUID = types.UID("replacement")
		if validateBucketMigration(j) == nil {
			t.Fatalf("accepted replaced %s", phase)
		}
	}
}

type migrationFixture struct {
	r      *Reconciler
	f      *fleet.CelldFleet
	j      *lifecycleJournal
	res    *fleet.CelldStorageReservation
	reader *bucketReader
	w      *appsv1.Deployment
	pods   *corev1.PodList
}

func migrationSetup(t *testing.T) *migrationFixture {
	t.Helper()
	p, f, j, opts, reader := bucketPreflightSetup(t)
	p.now = func() time.Time { return reader.now }
	p.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return maintenanceReader{Reader: reader}, nil
	}
	f.Spec.Replicas = 3
	j.Version = 7
	j.RuntimeImage = Image
	j.Initial = 3
	j.Applied = 3
	j.Operation = nil
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleet.ReservationSpec{InitialReplicas: 3, Bucket: f.Spec.Storage.Bucket, FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}}
	w := workload(f, opts).(*appsv1.Deployment)
	w.UID = j.WorkloadUID
	for _, o := range []client.Object{res, w} {
		if err := p.client.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	pods := &corev1.PodList{}
	if err := p.client.List(t.Context(), pods, client.InNamespace(f.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		pods.Items[i].Status.PodIP = "10.0.0." + string(rune('0'+i))
		if err := p.client.Status().Update(t.Context(), &pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	f.Spec.BucketWorkload = "Ordered"
	f.Spec.Maintenance = &fleet.MaintenanceSpec{AllowCoordinatedDowntime: true, OrderedMigrationToken: "ordered-1"}
	if err := p.client.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: p.client, Options: opts, Evidence: p, now: func() time.Time { return reader.now }}
	if err := r.saveJournal(t.Context(), res, j); err != nil {
		t.Fatal(err)
	}

	observation := capacity.Observation{At: reader.now, Complete: true}
	for i := range pods.Items {
		id, _ := podIdentity(&pods.Items[i])
		observation.Samples = append(observation.Samples, capacity.Sample{Identity: id, Ready: true, CPU: 10, MemoryMiB: 100, RuntimeAt: reader.now, RuntimeReceived: reader.now, MetricsAt: reader.now, MetricsReceived: reader.now, Window: 15 * time.Second})
	}
	r.Collector = bucketCapacityCollector{observation}
	f.Finalizers = []string{Finalizer}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	return &migrationFixture{r: r, f: f, j: j, res: res, reader: reader, w: w, pods: pods}
}
func (p *migrationFixture) step(t *testing.T) {
	t.Helper()
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), p.f); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.res), p.res); err != nil {
		t.Fatal(err)
	}
	_, handled, err := p.r.migrateBucket(t.Context(), p.f, p.res)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if err = p.r.Get(t.Context(), client.ObjectKeyFromObject(p.res), p.res); err != nil {
		t.Fatal(err)
	}
	p.j, err = p.r.loadJournal(t.Context(), p.res)
	if err != nil {
		t.Fatal(err)
	}
}
func (p *migrationFixture) advance(t *testing.T, phase string) {
	t.Helper()
	for range 15 {
		if p.j.BucketMigration != nil && p.j.BucketMigration.Phase == phase {
			return
		}
		if p.j.BucketMigration != nil && p.j.BucketMigration.Phase == "Recovering" {
			for i := range p.pods.Items {
				if err := p.r.Delete(t.Context(), &p.pods.Items[i]); err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			}
			p.reader.expired = map[string]bool{"pod-0": true, "pod-1": true, "pod-2": true}
			p.reader.now = p.reader.now.Add(11 * time.Second)
		}
		p.step(t)
	}
	t.Fatalf("did not reach %s: %+v", phase, p.j.BucketMigration)
}
func (p *migrationFixture) deleting(t *testing.T) {
	t.Helper()
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), p.f); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Delete(t.Context(), p.f); err != nil {
		t.Fatal(err)
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), p.f); err != nil {
		t.Fatal(err)
	}
}

func TestBucketMigrationDeletionDuringEveryPrecreationPhase(t *testing.T) {
	for _, phase := range []string{"Capture", "Authorized", "Recovering", "DeleteOld", "CreateNew"} {
		t.Run(phase, func(t *testing.T) {
			p := migrationSetup(t)
			p.advance(t, phase)
			p.deleting(t)
			p.advance(t, "Retained")
			p.step(t)
			if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), &fleet.CelldFleet{}); !apierrors.IsNotFound(err) {
				t.Fatal("deletion did not finalize", err)
			}
			if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("deletion created target", err)
			}
			if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.res), &fleet.CelldStorageReservation{}); err != nil {
				t.Fatal("reservation lost", err)
			}
		})
	}
}

func TestBucketMigrationPauseFinishesAdmittedRecovery(t *testing.T) {
	p := migrationSetup(t)
	p.advance(t, "Authorized")
	p.f.Spec.Maintenance.Paused = true
	if err := p.r.Update(t.Context(), p.f); err != nil {
		t.Fatal(err)
	}
	p.advance(t, "CreateNew")
	p.step(t)
	if !p.j.BucketMigration.CreationAuthorized {
		t.Fatal("pause stranded admitted migration")
	}
}

func TestBucketMigrationDeleteAfterCreationAuthorizationKeepsRecoveryAuthority(t *testing.T) {
	p := migrationSetup(t)
	p.advance(t, "CreateNew")
	p.step(t)
	p.deleting(t)
	target := workload(p.f, p.r.Options).(*appsv1.StatefulSet)
	setReplicas(target, 0)
	target.UID = "authorized-target"
	target.Annotations = map[string]string{migrationKey: p.j.BucketMigration.ID}
	if err := p.r.Create(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	p.step(t)
	if p.j.BucketMigration.Phase != "Activating" || p.j.BucketMigration.TargetUID != target.UID {
		t.Fatal("created target escaped deletion authority")
	}
	p.step(t)
	if p.j.BucketMigration.Phase != "Retained" {
		t.Fatal("inert target not retired")
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), &fleet.CelldFleet{}); err != nil {
		t.Fatal("finalized before target retirement", err)
	}
}

func TestBucketMigrationInterruptedSettlingRestartsWindow(t *testing.T) {
	p := migrationSetup(t)
	p.advance(t, "Recovering")
	for i := range p.pods.Items {
		if err := p.r.Delete(t.Context(), &p.pods.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	p.reader.expired = map[string]bool{"pod-0": true, "pod-1": true, "pod-2": true}
	p.step(t)
	if p.j.BucketMigration.SettledAt.IsZero() {
		t.Fatal("no first observation")
	}
	original := p.r.Evidence.reader
	p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
		return nil, errors.New("S3 unavailable")
	}
	p.reader.now = p.reader.now.Add(time.Minute)
	p.step(t)
	if !p.j.BucketMigration.SettledAt.IsZero() {
		t.Fatal("failed observation retained settling window")
	}
	p.r.Evidence.reader = original
	p.step(t)
	if p.j.BucketMigration.Phase != "Recovering" || !p.j.BucketMigration.SettledAt.Equal(p.reader.now) {
		t.Fatal("did not restart settling")
	}
}

type migrationLossReader struct{ v050.Reader }

func (r migrationLossReader) List(ctx context.Context, prefix, cursor string) (v050.Page, error) {
	if prefix == "log/" {
		return v050.Page{Keys: []string{"log/old/g.e1.loss.json"}, Complete: true}, nil
	}
	return r.Reader.List(ctx, prefix, cursor)
}
func TestBucketMigrationLateLossIsStickyAtComputeBoundaries(t *testing.T) {
	for _, phase := range []string{"Authorized", "DeleteOld", "CreateNew"} {
		t.Run(phase, func(t *testing.T) {
			p := migrationSetup(t)
			p.advance(t, phase)
			p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
				return migrationLossReader{maintenanceReader{Reader: p.reader}}, nil
			}
			p.step(t)
			if p.j.Loss == "" {
				t.Fatal("loss observation not persisted")
			}
			p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) {
				return maintenanceReader{Reader: p.reader}, nil
			}
			p.step(t)
			if p.j.BucketMigration.Phase != phase || p.j.Loss == "" {
				t.Fatal("missing marker erased loss authority")
			}
		})
	}
}

func TestBucketMigrationDeletionBeforeAdmissionUsesSourceLayout(t *testing.T) {
	p := migrationSetup(t)
	if p.res.Annotations == nil {
		p.res.Annotations = map[string]string{}
	}
	p.res.Annotations[attemptAnnotation] = "recorded"
	if err := p.r.Update(t.Context(), p.res); err != nil {
		t.Fatal(err)
	}
	p.deleting(t)
	for range 20 {
		if p.j.Maintenance != nil && p.j.Maintenance.Phase == "Recovering" {
			for i := range p.pods.Items {
				if err := p.r.Delete(t.Context(), &p.pods.Items[i]); err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			}
			p.reader.expired = map[string]bool{"pod-0": true, "pod-1": true, "pod-2": true}
			p.reader.now = p.reader.now.Add(11 * time.Second)
		}
		p.step(t)
		if p.j.BucketMigration != nil {
			t.Fatal("deletion admitted a new migration")
		}
		if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.f), &fleet.CelldFleet{}); apierrors.IsNotFound(err) {
			return
		}
	}
	t.Fatalf("source-layout deletion did not complete: %+v status %+v", p.j.Maintenance, p.f.Status.Conditions)
}

func TestBucketMigrationCannotActivateReplacedTarget(t *testing.T) {
	p := migrationSetup(t)
	p.advance(t, "CreateNew")
	p.step(t)
	target := workload(p.f, p.r.Options).(*appsv1.StatefulSet)
	setReplicas(target, 0)
	target.UID = "target-original"
	target.Annotations = map[string]string{migrationKey: p.j.BucketMigration.ID}
	if err := p.r.Create(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	p.step(t)
	if p.j.BucketMigration.Phase != "Activating" {
		t.Fatal("target UID not captured")
	}
	if err := p.r.Delete(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	target.UID = "target-replaced"
	target.ResourceVersion = ""
	if err := p.r.Create(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.r.migrateBucket(t.Context(), p.f, p.res); err == nil {
		t.Fatal("activated another UID")
	}
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(target), target); err != nil {
		t.Fatal(err)
	}
	if replicas(target) != 0 {
		t.Fatal("replacement launched replicas")
	}
}

func TestBucketMigrationUnknownWriterRemainsBlockedAfterMetadataDisappears(t *testing.T) {
	p := migrationSetup(t)
	p.advance(t, "CreateNew")
	p.reader.nodes = append(p.reader.nodes, "unadmitted")
	p.step(t)
	p.reader.nodes = p.reader.nodes[:len(p.reader.nodes)-1]
	p.step(t)
	if p.j.BucketMigration.CreationAuthorized {
		t.Fatal("unknown historical writer forgotten after GC")
	}
}
