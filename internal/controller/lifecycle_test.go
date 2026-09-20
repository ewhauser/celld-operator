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
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type localEvidence struct {
	validateAdvance                                        time.Duration
	now                                                    time.Time
	sessions                                               []v050.Session
	stopped, incomplete, loss, replaced, unavailable, omit bool
}

func (e *localEvidence) Now() time.Time                       { return e.now }
func (e *localEvidence) Reader(*fleet.CelldFleet) v050.Reader { return e }
func (e *localEvidence) Capture(_ context.Context, f *fleet.CelldFleet, ordinal int32, prior []v050.Session) (*lifecycleOperation, error) {
	if e.sessions == nil {
		for i := int32(0); i < f.Spec.Replicas+1; i++ {
			e.sessions = append(e.sessions, v050.Session{Node: fmt.Sprintf("%s-%d", f.Name, i), Generation: "gen", Epoch: 1})
		}
	}
	sessions := append([]v050.Session(nil), e.sessions...)
	for _, p := range prior {
		for i := range sessions {
			if sessions[i].Node == p.Node {
				sessions[i] = p
			}
		}
	}
	if e.omit {
		sessions = sessions[:1]
	}
	return &lifecycleOperation{TargetPod: fmt.Sprintf("%s-%d", f.Name, ordinal), TargetUID: "pod-uid", TargetGeneration: "gen", Sessions: sessions}, nil
}
func (e *localEvidence) Validate(context.Context, *fleet.CelldFleet, *lifecycleOperation) error {
	e.now = e.now.Add(e.validateAdvance)
	if e.unavailable {
		return errors.New("survivor unavailable")
	}
	return nil
}
func (e *localEvidence) Stopped(context.Context, *fleet.CelldFleet, *lifecycleOperation) (bool, error) {
	return e.stopped, nil
}
func (e *localEvidence) List(_ context.Context, prefix, _ string) (v050.Page, error) {
	page := v050.Page{Complete: !e.incomplete}
	if prefix == "nodes/" {
		for _, s := range e.sessions {
			page.Keys = append(page.Keys, "nodes/"+s.Node+".json")
		}
	}
	if prefix == "log/" && e.loss {
		page.Keys = []string{"log/old/old.bundle-tail.loss.json"}
	}
	return page, nil
}
func (e *localEvidence) Get(_ context.Context, key string) ([]byte, error) {
	for _, s := range e.sessions {
		if key == "nodes/"+s.Node+".json" {
			gen := s.Generation
			if e.replaced {
				gen = "replacement"
			}
			return json.Marshal(map[string]any{"node": s.Node, "ownership_index_generation": gen, "expires_ms": e.now.Add(time.Hour).UnixMilli(), "peer_protocol": 5, "log": map[string]any{"active": false, "state": "sealed", "epoch": 1, "ensemble": []string{}, "tiered": 1}})
		}
	}
	return nil, errors.New("missing node")
}
func getJournal(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *lifecycleJournal {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(res)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// recordedEvents drains everything a fake recorder has collected so far. The
// audit trail that History no longer keeps is emitted as events instead, so
// tests assert on this rather than on a growing journal field.
func recordedEvents(recorder *events.FakeRecorder) []string {
	notes := []string{}
	for {
		select {
		case note := <-recorder.Events:
			notes = append(notes, note)
		default:
			return notes
		}
	}
}

// recordedEvent reports whether any drained event mentions every given fragment.
func recordedEvent(recorder *events.FakeRecorder, fragments ...string) bool {
	for _, note := range recordedEvents(recorder) {
		found := true
		for _, fragment := range fragments {
			if !strings.Contains(note, fragment) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

func desiredCount(t *testing.T, r *Reconciler, f *fleet.CelldFleet, n int32) *fleet.CelldFleet {
	t.Helper()
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = n
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	return got
}
func lifecycleSetup(t *testing.T, profile string) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", profile)
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	reconcile(t, r, f)
	reconcile(t, r, f)
	return r, f
}

// TestCompletionHistoryKeepsOnlyTheLastEntry pins the ADR 0021 phase 1 boundary
// for History: the journal retains exactly the entry the status projection
// reads, and the trail that used to grow beside it is emitted as events.
func TestCompletionHistoryKeepsOnlyTheLastEntry(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	recorder := events.NewFakeRecorder(64)
	r.Recorder = recorder
	f = desiredCount(t, r, f, 4)
	for range 3 {
		reconcile(t, r, f)
	}
	j := getJournal(t, r, f)
	if j.Applied != 4 || len(j.History) != 1 || j.History[0].From != 3 || j.History[0].To != 4 {
		t.Fatalf("first expansion: %+v", j)
	}
	first := j.History[0].ID
	f = desiredCount(t, r, f, 5)
	for range 3 {
		reconcile(t, r, f)
	}
	j = getJournal(t, r, f)
	if j.Applied != 5 || len(j.History) != 1 || j.History[0].From != 4 || j.History[0].To != 5 || j.History[0].ID == first {
		t.Fatalf("second expansion did not replace the retained entry: %+v", j)
	}
	notes := recordedEvents(recorder)
	for _, id := range []string{first, j.History[0].ID} {
		if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, id) }) {
			t.Fatalf("completion %s never reached the event stream: %v", id, notes)
		}
	}
}

// TestBootstrapConsumesCreationClaimInventory pins the ADR 0021 phase 1
// boundary for the creation claim annotation: bootstrap is its only reader, so
// the write that first persists the journal removes it, later reconciles still
// load, and the crash window it guards still blocks for review.
func TestBootstrapConsumesCreationClaimInventory(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet") // create, then bootstrap
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if _, present := res.Annotations[creationClaimsKey]; present {
		t.Fatalf("bootstrap retained the consumed claim inventory: %v", res.Annotations)
	}
	j, err := readJournal(res)
	if err != nil || j == nil || len(j.Claims) != 3 {
		t.Fatalf("bootstrap did not carry the claim identities into the journal: %v %+v", err, j)
	}
	reason(t, reconcile(t, r, f), "Provisioning") // waiting on readiness, not blocked
	if after := getJournal(t, r, f); after == nil || len(after.Claims) != 3 {
		t.Fatalf("journal no longer loads without the annotation: %+v", after)
	}

	// A crash between PVC creation and bootstrap leaves a workload with neither
	// a journal nor an inventory to verify it against; that still blocks.
	other := fixture("beta", "bucket-beta", "PersistentFleet")
	other.Spec.Placement.AZCount = 1
	other.Spec.Placement.Zones = []string{"us-east-1a"}
	x := setup(t, other)
	reconcile(t, x, other)
	crashed := &fleet.CelldStorageReservation{}
	if err := x.Get(t.Context(), types.NamespacedName{Name: reservationName(other)}, crashed); err != nil {
		t.Fatal(err)
	}
	delete(crashed.Annotations, creationClaimsKey)
	if err := x.Update(t.Context(), crashed); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, x, other), "StorageIdentityConflict")
	if j := getJournal(t, x, other); j != nil {
		t.Fatalf("unverified workload was adopted: %+v", j)
	}
}

func TestLifecycleScaleOutRestartsAndStatusLoss(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = desiredCount(t, r, f, 5)
			for range 6 {
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true}
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Applied != 5 || j.Operation != nil {
				t.Fatalf("not completed: %+v", j)
			}
			f = &fleet.CelldFleet{Name: f.Name, Namespace: f.Namespace}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
				t.Fatal(err)
			}
			f.Status = fleet.CelldFleetStatus{}
			if err := r.Status().Update(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, f)
			if getJournal(t, r, f).Applied != 5 {
				t.Fatal("status loss erased capacity")
			}
			if profile == "PersistentFleet" && len(j.Claims) != 5 {
				t.Fatal("missing retained PVC identities")
			}
		})
	}
}
func TestLifecycleContractionQualificationBlocks(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := lifecycleSetup(t, profile)
			f = desiredCount(t, r, f, 1)
			reconcile(t, r, f)
			j := getJournal(t, r, f)
			if j.Operation == nil || j.Operation.Phase != "Blocked" || j.Applied != 3 {
				t.Fatal("unqualified contraction issued")
			}
		})
	}
}
func TestLifecycleRepeatedContractionAndCrashAfterEffect(t *testing.T) {
	e := &localEvidence{now: time.Now(), stopped: true}
	// Local workload configuration must match the explicitly selected test mode.
	// Recreate the fixture with LocalTest before initial provisioning.
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.Options.LocalTest = true
	r.localLifecycle = e
	reconcile(t, r, f)
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	old := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), old); err != nil {
		t.Fatal(err)
	}
	oldOp := *getJournal(t, r, f).Operation
	base := r.Client
	once := true
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok && once {
			once = false
			return errors.New("leader crashed after replica CAS")
		}
		return c.Update(ctx, obj, opts...)
	}})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err == nil {
		t.Fatal("crash not injected")
	}
	f = desiredCount(t, r, f, 3) // A reversal after the effect must not strand recovery.
	r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true, localLifecycle: e}
	for range 3 {
		reconcile(t, r, f)
	}
	e.now = e.now.Add(11 * time.Second)
	reconcile(t, r, f)
	if j := getJournal(t, r, f); j.Applied != 2 || j.Operation != nil {
		t.Fatalf("recovery incomplete: %+v", j)
	}
	f = desiredCount(t, r, f, 1)
	for range 3 {
		reconcile(t, r, f)
	}
	e.now = e.now.Add(11 * time.Second)
	reconcile(t, r, f)
	j := getJournal(t, r, f)
	// History keeps the last completion only; the earlier one is an event.
	if j.Applied != 1 || len(j.Claims) != 3 || len(j.Sessions) != 3 || len(j.History) != 1 || j.History[0].EvidenceAt.IsZero() || j.History[0].TargetPod != "alpha-1" {
		t.Fatalf("history lost: %+v", j)
	}
	if err := r.applyReplicas(t.Context(), old, &oldOp); !apierrors.IsConflict(err) {
		t.Fatalf("old leader was not fenced by CAS: %v", err)
	}
}
func TestLifecycleEvidenceFaultsRetainOperation(t *testing.T) {
	for _, fault := range []string{"incomplete", "generation", "loss", "unfenced", "survivor"} {
		t.Run(fault, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "PersistentFleet")
			f.Spec.Placement.AZCount = 1
			f.Spec.Placement.Zones = []string{"us-east-1a"}
			r := setup(t, f)
			r.Options.LocalTest = true
			e := &localEvidence{now: time.Now(), stopped: true}
			r.localLifecycle = e
			reconcile(t, r, f)
			reconcile(t, r, f)
			f = desiredCount(t, r, f, 2)
			reconcile(t, r, f)
			reconcile(t, r, f)
			id := getJournal(t, r, f).Operation.ID
			switch fault {
			case "incomplete":
				e.incomplete = true
			case "generation":
				e.replaced = true
			case "loss":
				e.loss = true
			case "unfenced":
				e.stopped = false
			case "survivor":
				e.unavailable = true
			}
			for range 3 {
				e.now = e.now.Add(time.Hour)
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true, localLifecycle: e}
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Applied != 3 || j.Operation == nil || j.Operation.ID != id {
				t.Fatal("uncertainty completed a removal")
			}
			if fault == "loss" {
				e.loss = false
				reconcile(t, r, f)
				if getJournal(t, r, f).Loss == "" {
					t.Fatal("loss finding forgotten")
				}
			}
		})
	}
}
func TestLifecycleRetainedPVCReplacementBlocks(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	pvc := &corev1.PersistentVolumeClaim{Name: "data-alpha-0", Namespace: f.Namespace}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), pvc); err != nil {
		t.Fatal(err)
	}
	f = desiredCount(t, r, f, 4)
	reason(t, reconcile(t, r, f), "StorageIdentityConflict")
}

// The listed retained inventory must refuse exactly what a per-claim Get
// refused: a replaced UID, a claim being deleted, a claim that left the fleet's
// labels, an adopted claim and a re-pointed reservation annotation.
func TestLifecycleRetainedClaimIdentityChecks(t *testing.T) {
	cases := map[string]func(*testing.T, *Reconciler, *fleet.CelldFleet, *corev1.PersistentVolumeClaim){
		"missing": func(t *testing.T, r *Reconciler, _ *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			if err := r.Delete(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
		},
		"wrong uid": func(t *testing.T, r *Reconciler, f *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			j, err := readJournal(res)
			if err != nil {
				t.Fatal(err)
			}
			j.Claims[pvc.Name] = "replaced-disk-uid"
			if err := r.saveJournal(t.Context(), res, j); err != nil {
				t.Fatal(err)
			}
		},
		"deleted": func(t *testing.T, r *Reconciler, _ *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			pvc.Finalizers = append(pvc.Finalizers, "example.com/hold")
			if err := r.Update(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
		},
		"label mismatch": func(t *testing.T, r *Reconciler, _ *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			pvc.Labels[FleetLabel] = "other-fleet-uid"
			if err := r.Update(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
		},
		"adopted": func(t *testing.T, r *Reconciler, _ *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			pvc.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "old", UID: "old"}}
			if err := r.Update(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
		},
		"foreign reservation": func(t *testing.T, r *Reconciler, _ *fleet.CelldFleet, pvc *corev1.PersistentVolumeClaim) {
			pvc.Annotations["celld.eric.dev/storage-reservation"] = "someone-elses-reservation"
			if err := r.Update(t.Context(), pvc); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, f := lifecycleSetup(t, "PersistentFleet")
			pvc := &corev1.PersistentVolumeClaim{Name: "data-alpha-0", Namespace: f.Namespace}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc); err != nil {
				t.Fatal(err)
			}
			mutate(t, r, f, pvc)
			reason(t, reconcile(t, r, f), "StorageIdentityConflict")
		})
	}
}

// Retained claim revalidation costs one List per reconcile, never one Get per
// journaled claim: the direct client is uncached, so a large fleet used to pay
// a live API round trip per disk every few seconds.
func TestLifecycleRetainedClaimsCostOneList(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Replicas = 12
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	reconcile(t, r, f)
	reconcile(t, r, f)
	if got := len(getJournal(t, r, f).Claims); got != 12 {
		t.Fatalf("journal records %d claims, want 12", got)
	}
	var gets, lists int
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
				gets++
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PersistentVolumeClaimList); ok {
				lists++
			}
			return c.List(ctx, list, opts...)
		},
	})
	reconcile(t, r, f)
	r.Client = base
	if gets != 0 || lists != 1 {
		t.Fatalf("idle reconcile of a 12-replica fleet issued %d PVC Gets and %d PVC Lists, want 0 and 1", gets, lists)
	}
}

func TestLifecycleConcurrentIntentAndReplicaCAS(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = desiredCount(t, r, f, 5)
	// Two leaders with the same reservation version cannot both choose intent.
	resA := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, resA); err != nil {
		t.Fatal(err)
	}
	resB := resA.DeepCopy()
	j, _ := readJournal(resA)
	j.Operation = &lifecycleOperation{ID: "first", Phase: "Intent", From: 3, To: 5}
	if err := r.saveJournal(t.Context(), resA, j); err != nil {
		t.Fatal(err)
	}
	j.Operation.ID = "second"
	if err := r.saveJournal(t.Context(), resB, j); !apierrors.IsConflict(err) {
		t.Fatalf("second leader won: %v", err)
	}
	for range 5 {
		reconcile(t, r, f)
	}
	if getJournal(t, r, f).Applied != 5 {
		t.Fatal("winning operation did not resume")
	}
}

func TestLifecycleDesiredChangesKeepRecordedIntent(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	f = desiredCount(t, r, f, 5)
	reconcile(t, r, f)
	id := getJournal(t, r, f).Operation.ID
	f = desiredCount(t, r, f, 2)
	for range 5 {
		reconcile(t, r, f)
	}
	j := getJournal(t, r, f)
	if j.Applied != 5 || j.Operation == nil || j.Operation.Phase != "Blocked" {
		t.Fatal("additive intent retargeted")
	}
	w := &appsv1.Deployment{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if w.Annotations[operationKey] != id {
		t.Fatal("operation identity changed")
	}
	reason(t, reconcile(t, r, f), "BucketCompletionUnqualified")
}

func TestLifecycleCrashDuringPVCAllocationRetainsUnknownDisk(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	f = desiredCount(t, r, f, 4)
	reconcile(t, r, f)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok {
			return errors.New("crash after PVC creation")
		}
		return c.Update(ctx, obj, opts...)
	}})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err == nil {
		t.Fatal("crash not injected")
	}
	r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true}
	reason(t, reconcile(t, r, f), "ScaleOutBlocked")
	if err := r.Get(t.Context(), types.NamespacedName{Namespace: f.Namespace, Name: "data-alpha-3"}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatal("claim was not retained", err)
	}
	if getJournal(t, r, f).Applied != 3 {
		t.Fatal("unsafe disk identity adopted")
	}
}

func TestLifecycleSettlementUncertaintyRestartsWindow(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.Options.LocalTest = true
	e := &localEvidence{now: time.Now(), stopped: true}
	r.localLifecycle = e
	reconcile(t, r, f)
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	for range 3 {
		reconcile(t, r, f)
	}
	if getJournal(t, r, f).Operation.SettledAt.IsZero() {
		t.Fatal("first assessment not recorded")
	}
	e.unavailable = true
	e.now = e.now.Add(time.Hour)
	reconcile(t, r, f)
	if !getJournal(t, r, f).Operation.SettledAt.IsZero() {
		t.Fatal("uncertainty retained settle credit")
	}
	e.unavailable = false
	reconcile(t, r, f)
	if getJournal(t, r, f).Operation == nil {
		t.Fatal("elapsed uncertainty completed operation")
	}
}

func TestLifecycleMissingHistoricalSessionBlocksNextRemoval(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.Options.LocalTest = true
	e := &localEvidence{now: time.Now(), stopped: true}
	r.localLifecycle = e
	reconcile(t, r, f)
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	for range 3 {
		reconcile(t, r, f)
	}
	e.now = e.now.Add(11 * time.Second)
	reconcile(t, r, f)
	e.omit = true
	f = desiredCount(t, r, f, 1)
	reason(t, reconcile(t, r, f), "RecoveryBlocked")
	if getJournal(t, r, f).Operation != nil {
		t.Fatal("forgot historical inventory")
	}
}

func TestLifecycleSlowMembershipCheckExpiresPreflight(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.Options.LocalTest = true
	e := &localEvidence{now: time.Now(), stopped: true, validateAdvance: 6 * time.Second}
	r.localLifecycle = e
	reconcile(t, r, f)
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	reason(t, reconcile(t, r, f), "RecoveryBlocked")
	w := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if *w.Spec.Replicas != 3 {
		t.Fatal("expired evidence authorized removal")
	}
}

// reservationWrites counts reservation Updates, which is the etcd write rate a
// steady-state reconcile costs: saveJournal is the only writer of that object
// once provisioning has recorded its creation attempt.
func reservationWrites(t *testing.T, r *Reconciler) *int {
	t.Helper()
	writes := 0
	base, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("test client cannot be intercepted")
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*fleet.CelldStorageReservation); ok {
			writes++
		}
		return c.Update(ctx, obj, opts...)
	}})
	return &writes
}

func TestSteadyEvidenceObservationSkipsUnchangedJournalWrites(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	now := time.Unix(1_700_000_000, 0).UTC()
	r.now = func() time.Time { return now }
	source := &inventoryReader{node: "alpha-0", gen: "generation", epoch: 1, now: now}
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	advance := func(d time.Duration) {
		now = now.Add(d)
		source.now = now
	}
	writes := reservationWrites(t, r)
	// The first observation records a new session and must be durable.
	reconcile(t, r, f)
	if *writes != 1 {
		t.Fatalf("first observation wrote %d times", *writes)
	}
	checked := getJournal(t, r, f).Inventory.CheckedAt
	if checked.IsZero() {
		t.Fatal("inventory not journaled")
	}
	// An idle pass moves only LastSeen and CheckedAt: nothing decided, no write.
	advance(6 * time.Second)
	reconcile(t, r, f)
	advance(6 * time.Second)
	reconcile(t, r, f)
	if *writes != 1 {
		t.Fatalf("idle reconciles wrote the reservation %d times", *writes)
	}
	if !getJournal(t, r, f).Inventory.CheckedAt.Equal(checked) {
		t.Fatal("a skipped observation still changed the durable journal")
	}
	// A new session is evidence, not a timestamp: it must survive a crash.
	advance(6 * time.Second)
	source.gen = "successor"
	reconcile(t, r, f)
	if *writes != 2 {
		t.Fatalf("new session wrote %d times", *writes)
	}
	if len(getJournal(t, r, f).Inventory.Sessions) != 2 {
		t.Fatal("new session not journaled")
	}
	advance(6 * time.Second)
	reconcile(t, r, f)
	if *writes != 2 {
		t.Fatalf("idle reconcile after a new session wrote %d times", *writes)
	}
	// status.lifecycle.evidenceCheckedAt is projected from the durable journal,
	// so the heartbeat keeps the reported observation age honest.
	advance(evidenceHeartbeat)
	reconcile(t, r, f)
	if *writes != 3 {
		t.Fatalf("heartbeat wrote %d times", *writes)
	}
	if !getJournal(t, r, f).Inventory.CheckedAt.Equal(now) {
		t.Fatal("heartbeat did not refresh the durable observation")
	}
}

func TestPendingOperationPersistsEveryObservation(t *testing.T) {
	r, f := lifecycleSetup(t, "PersistentFleet")
	now := time.Unix(1_700_000_000, 0).UTC()
	r.now = func() time.Time { return now }
	source := &inventoryReader{node: "alpha-0", gen: "generation", epoch: 1, now: now}
	r.Evidence = &ProductionEvidence{client: r.Client, now: r.now, reader: func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return source, nil }}
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	if op := getJournal(t, r, f).Operation; op == nil || op.Phase != "Blocked" {
		t.Fatalf("no operation in flight: %+v", getJournal(t, r, f).Operation)
	}
	writes := reservationWrites(t, r)
	for pass := range 2 {
		now = now.Add(6 * time.Second)
		source.now = now
		reconcile(t, r, f)
		if *writes <= pass {
			t.Fatalf("observation under a recorded operation was not persisted on pass %d", pass)
		}
	}
}
