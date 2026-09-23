package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const seedReservationCreated = "celld.eric.dev/seed-reservation-created"
const seedGate = "celld.eric.dev/seed-gate"
const seedGateCanceled = "canceled"

func seedTerminal(s *fleet.CelldStorageReservation) bool {
	return s.Status.Phase == "Succeeded" || s.Status.Phase == "Failed" || s.Status.Phase == "Canceled"
}

func seedSource(pool *fleet.CelldFleet, alias string) (fleet.SeedFleetReference, error) {
	if pool.Spec.Previews.Seeding != nil && pool.Spec.Previews.Seeding.Executor != "" {
		for _, s := range pool.Spec.Previews.Seeding.Sources {
			if s.Name == alias {
				return s.FleetRef, nil
			}
		}
	}
	return fleet.SeedFleetReference{}, fmt.Errorf("seed source %q is not authorized by this pool", alias)
}

func (r *PreviewReconciler) previewSeedRequest(ctx context.Context, p *fleet.CelldPreview, pool, f *fleet.CelldFleet, expires time.Time) (*fleet.PreviewSeedRequest, error) {
	source, err := seedSource(pool, p.Spec.Seed.Source)
	if err != nil {
		return nil, err
	}
	selection := p.Spec.Seed.DeepCopy()
	slices.SortFunc(selection.Objects, func(a, b fleet.PreviewObjectReference) int {
		if n := cmp.Compare(a.Class, b.Class); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if selection.Alarms == "" {
		selection.Alarms = "Clear"
	}
	want := fleet.PreviewSeedRequest{Executor: pool.Spec.Previews.Seeding.Executor, SourceFleet: source, Selection: *selection, Deadline: metav1.NewTime(expires), Target: fleet.PreviewSeedTarget{PreviewName: p.Name, PreviewUID: string(p.UID), FleetName: f.Name, PreviewFleetRef: *f.Spec.Storage.PreviewFleetRef, StorageURL: f.Spec.Storage.URL()}}
	// Existing children already pin their authorized request. Revisions never reseed.
	if p.Annotations[previewCreated] == "" {
		src := &fleet.CelldFleet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, src); err != nil {
			return nil, fmt.Errorf("seed source fleet: %w", err)
		}
		if string(src.UID) != source.UID || !src.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("seed source fleet identity changed or is deleting")
		}
		if err := src.ValidateRuntime(); err != nil {
			return nil, fmt.Errorf("seed source configuration: %w", err)
		}
		if err := verifySharedStorage(ctx, r.Client, src); err != nil {
			return nil, err
		}
		reservation := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKey{Name: reservationName(src)}, reservation); err != nil {
			return nil, fmt.Errorf("seed source reservation: %w", err)
		}
		original := src.DeepCopy()
		if reservation.Spec.InitialReplicas > 0 {
			original.Spec.Replicas = reservation.Spec.InitialReplicas
		}
		if !equality.Semantic.DeepEqual(reservation.Spec, fleetReservationSpec(original)) || len(reservation.OwnerReferences) != 0 || !reservation.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("seed source storage authority does not match the authorized fleet")
		}
	}
	return &want, nil
}

// ensureSeedReservation pins the destination's retained record before any worker
// may claim it. A missing record after creation intent is never recreated.
func ensureSeedReservation(ctx context.Context, c client.Client, f *fleet.CelldFleet) (*fleet.CelldStorageReservation, error) {
	res := &fleet.CelldStorageReservation{}
	err := c.Get(ctx, client.ObjectKey{Name: reservationName(f)}, res)
	if apierrors.IsNotFound(err) {
		if f.Annotations[seedReservationCreated] != "" {
			return nil, fmt.Errorf("seed reservation missing after creation intent")
		}
		before := f.DeepCopy()
		if f.Annotations == nil {
			f.Annotations = map[string]string{}
		}
		f.Annotations[seedReservationCreated] = "true"
		if err := c.Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
		res = &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)}
		if err := c.Create(ctx, res); err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
				before := f.DeepCopy()
				delete(f.Annotations, seedReservationCreated)
				if patchErr := c.Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); patchErr != nil {
					return nil, patchErr
				}
			}
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if res.UID == "" || !equality.Semantic.DeepEqual(res.Spec, fleetReservationSpec(f)) || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("seed reservation authority conflict")
	}
	identity := f.Annotations[seedReservationCreated]
	if identity != "" && identity != "true" && identity != string(res.UID) {
		return nil, fmt.Errorf("seed reservation UID changed")
	}
	if identity != string(res.UID) {
		before := f.DeepCopy()
		if f.Annotations == nil {
			f.Annotations = map[string]string{}
		}
		f.Annotations[seedReservationCreated] = string(res.UID)
		if err := c.Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// cancelSeed claims unclaimed requests with the same optimistic CAS required of
// executors. If the executor won Running first, only it may attest quiescence.
func cancelSeed(ctx context.Context, c client.Client, s *fleet.CelldStorageReservation) (bool, error) {
	if seedTerminal(s) {
		return true, nil
	}
	if !s.Status.Canceled {
		before := s.DeepCopy()
		s.Status.Canceled = true
		if err := c.Status().Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	if s.Status.Phase == "" || s.Status.Phase == "Pending" {
		before := s.DeepCopy()
		s.Status.Phase = "Canceled"
		s.Status.Message = "Canceled before executor claim"
		if err := c.Status().Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *PreviewReconciler) cancelPreviewSeed(ctx context.Context, p *fleet.CelldPreview, f *fleet.CelldFleet, missing bool) (bool, error) {
	if p.Spec.Seed == nil {
		return true, nil
	}
	// No child means no executor can claim a destination. After deletion, the
	// fleet finalizer has already verified quiescence in its retained reservation.
	if missing {
		name := p.Annotations[previewSeedReservation]
		if name == "" {
			if p.Annotations[previewCreated] != "" {
				return false, fmt.Errorf("missing seed reservation identity")
			}
			return true, nil
		}
		res := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, res); err != nil {
			return false, err
		}
		if res.Spec.Initialization == nil || res.Spec.Initialization.Target.PreviewUID != string(p.UID) || res.Spec.FleetNamespace != p.Namespace || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() {
			return false, fmt.Errorf("seed reservation identity conflict")
		}
		return cancelSeed(ctx, r.Client, res)
	}
	s, err := ensureSeedReservation(ctx, r.Client, f)
	if err != nil {
		return false, err
	}
	if _, err := getFleetSeed(ctx, r.Client, f); err != nil {
		return false, err
	}
	return cancelSeed(ctx, r.Client, s)
}

func getFleetSeed(ctx context.Context, c client.Client, f *fleet.CelldFleet) (*fleet.CelldStorageReservation, error) {
	request := f.Spec.Storage.Initialization
	if request == nil {
		return nil, fmt.Errorf("fleet has no initialization request")
	}
	s := &fleet.CelldStorageReservation{}
	if err := c.Get(ctx, client.ObjectKey{Name: reservationName(f)}, s); err != nil {
		return nil, err
	}
	target := request.Target
	owner := metav1.GetControllerOf(f)
	if !equality.Semantic.DeepEqual(s.Spec, fleetReservationSpec(f)) || f.Annotations[seedReservationCreated] != string(s.UID) || len(s.OwnerReferences) != 0 || !s.DeletionTimestamp.IsZero() || owner == nil || owner.Kind != "CelldPreview" || owner.APIVersion != fleet.GroupVersion.String() || string(owner.UID) != target.PreviewUID || owner.Name != target.PreviewName || target.FleetName != f.Name || target.StorageURL != f.Spec.Storage.URL() || f.Spec.Storage.PreviewFleetRef == nil || target.PreviewFleetRef != *f.Spec.Storage.PreviewFleetRef {
		return nil, fmt.Errorf("seed request does not bind this preview, fleet and storage identity")
	}
	return s, nil
}

func completedSeedReceipt(s *fleet.CelldStorageReservation, f *fleet.CelldFleet) (string, error) {
	if s.Spec.Initialization == nil || s.Status.Canceled || s.Status.Phase != "Succeeded" || s.Status.TargetFleetUID != string(f.UID) || s.Status.ExecutorID == "" || s.Status.Manifest == nil {
		return "", fmt.Errorf("seed has no successful completion for this fleet UID")
	}
	if err := s.Spec.Initialization.Selection.Validate(); err != nil {
		return "", err
	}
	expected := map[fleet.PreviewObjectReference]bool{}
	for _, o := range s.Spec.Initialization.Selection.Objects {
		expected[o] = true
	}
	for _, o := range s.Status.Manifest.Objects {
		if !expected[o.PreviewObjectReference] || o.SnapshotID == "" || len(o.SnapshotID) > 256 || o.SourceVersion == "" || len(o.SourceVersion) > 256 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(o.Digest) {
			return "", fmt.Errorf("seed manifest must contain exactly every selected object's snapshot, committed version and digest")
		}
		delete(expected, o.PreviewObjectReference)
	}
	if len(expected) != 0 {
		return "", fmt.Errorf("seed manifest is incomplete")
	}
	manifest := s.Status.Manifest.DeepCopy()
	slices.SortFunc(manifest.Objects, func(a, b fleet.PreviewObjectSnapshot) int {
		if n := cmp.Compare(a.Class, b.Class); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	b, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return "ready:" + s.Spec.Initialization.Target.PreviewUID + ":" + digest(b), nil
}

func seedReceiptReady(f *fleet.CelldFleet, res *fleet.CelldStorageReservation) bool {
	if f.Spec.Storage.Initialization == nil {
		return true
	}
	original := f.DeepCopy()
	if res.Spec.InitialReplicas > 0 {
		original.Spec.Replicas = res.Spec.InitialReplicas
	}
	if res.UID == "" || f.Annotations[seedReservationCreated] != string(res.UID) || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(res.Spec, fleetReservationSpec(original)) {
		return false
	}
	prefix := "ready:" + f.Spec.Storage.Initialization.Target.PreviewUID + ":"
	v := res.Annotations[seedGate]
	return strings.HasPrefix(v, prefix) && regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(strings.TrimPrefix(v, prefix))
}

// gateSeed is before ANY workload prerequisites or creation intent. The retained
// reservation CAS is the arbitration point between startup and cancellation.
func (r *Reconciler) gateSeed(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation) (bool, error) {
	if f.Spec.Storage.Initialization == nil || seedReceiptReady(f, res) {
		return true, nil
	}
	if res.Annotations[seedGate] != "" {
		return false, fmt.Errorf("seed startup gate is canceled or invalid")
	}
	s, err := getFleetSeed(ctx, r.Client, f)
	if err != nil {
		return false, err
	}
	receipt, err := completedSeedReceipt(s, f)
	if err != nil {
		return false, err
	}
	// Deadline gates initial startup, not an already-running preview's lifecycle.
	if !time.Now().Before(s.Spec.Initialization.Deadline.Time) {
		return false, fmt.Errorf("seed deadline elapsed before startup")
	}
	if res.Annotations == nil {
		res.Annotations = map[string]string{}
	}
	res.Annotations[seedGate] = receipt
	if err := r.Update(ctx, res); err != nil {
		return false, err
	}
	return true, nil
}

// deleteUninitializedFleet handles only fleets whose seed gate never opened.
// Once startup wins, the existing lifecycle journal/finalizer is authoritative.
func (r *Reconciler) deleteUninitializedFleet(ctx context.Context, f *fleet.CelldFleet) (bool, error) {
	if f.Spec.Storage.Initialization == nil {
		return false, nil
	}
	s, err := ensureSeedReservation(ctx, r.Client, f)
	if err != nil {
		return true, err
	}
	if _, err := getFleetSeed(ctx, r.Client, f); err != nil {
		return true, err
	}
	stopped, err := cancelSeed(ctx, r.Client, s)
	if err != nil {
		return true, err
	}
	if !stopped {
		return true, fmt.Errorf("waiting for seed executor to stop all writes")
	}
	res := s
	// A startup winner must use ordinary fleet shutdown; its retained receipt
	// cannot authorize the never-started deletion shortcut.
	if seedReceiptReady(f, res) {
		return false, nil
	}
	if !equality.Semantic.DeepEqual(res.Spec, fleetReservationSpec(f)) || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() || res.Annotations[attemptAnnotation] != "" || res.Annotations[stateKey] != "" {
		return true, fmt.Errorf("uninitialized fleet has conflicting storage or workload authority")
	}
	if res.Annotations[seedGate] != "" && res.Annotations[seedGate] != seedGateCanceled {
		return true, fmt.Errorf("invalid seed startup gate")
	}
	if res.Annotations[seedGate] != seedGateCanceled {
		if res.Annotations == nil {
			res.Annotations = map[string]string{}
		}
		res.Annotations[seedGate] = seedGateCanceled
		if err := r.Update(ctx, res); err != nil {
			return true, err
		}
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), w); err == nil {
		return true, fmt.Errorf("uninitialized fleet unexpectedly has a workload")
	} else if !apierrors.IsNotFound(err) {
		return true, fmt.Errorf("cannot prove absence of an uninitialized workload: %w", err)
	}
	before := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	return true, r.Patch(ctx, f, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
