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

const previewSeedCreated = "celld.eric.dev/preview-seed-created"
const seedGate = "celld.eric.dev/seed-gate"
const seedGateCanceled = "canceled"

func seedName(p *fleet.CelldPreview) string { return previewName(p) + "-seed" }
func seedTerminal(s *fleet.CelldPreviewSeed) bool {
	return s.Status.Phase == "Succeeded" || s.Status.Phase == "Failed" || s.Status.Phase == "Canceled"
}

func seedSource(pool *fleet.CelldPreviewPool, alias string) (fleet.SeedFleetReference, error) {
	if pool.Spec.Seeding != nil && pool.Spec.Seeding.Executor != "" {
		for _, s := range pool.Spec.Seeding.Sources {
			if s.Name == alias {
				return s.FleetRef, nil
			}
		}
	}
	return fleet.SeedFleetReference{}, fmt.Errorf("seed source %q is not authorized by this pool", alias)
}

func (r *PreviewReconciler) ensurePreviewSeed(ctx context.Context, p *fleet.CelldPreview, pool *fleet.CelldPreviewPool, f *fleet.CelldFleet, expires time.Time) (*fleet.CelldPreviewSeed, error) {
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
	want := fleet.PreviewSeedRequest{Executor: pool.Spec.Seeding.Executor, SourceFleet: source, Selection: *selection, Deadline: metav1.NewTime(expires), Target: fleet.PreviewSeedTarget{PreviewName: p.Name, PreviewUID: string(p.UID), FleetName: f.Name, PoolRef: *f.Spec.Storage.PoolRef, StorageURL: f.Spec.Storage.URL()}}
	s := &fleet.CelldPreviewSeed{}
	err = r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: seedName(p)}, s)
	if apierrors.IsNotFound(err) {
		if p.Annotations[previewSeedCreated] != "" {
			return nil, fmt.Errorf("seed request missing after creation intent; automatic recreation is forbidden")
		}
		// Verify the approved source identity and its retained storage authority. The
		// executor rechecks this before exporting; this read does not claim a snapshot.
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
		before := p.DeepCopy()
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[previewSeedCreated] = "true"
		p.Annotations[previewPoolUID] = string(pool.UID)
		scheme := pool.Spec.Routing.Scheme
		if scheme == "" {
			scheme = "https"
		}
		p.Annotations[previewURL] = scheme + "://" + f.Name + "." + pool.Spec.Routing.BaseDomain
		if err := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
		s = &fleet.CelldPreviewSeed{Name: seedName(p), Namespace: p.Namespace, Spec: fleet.CelldPreviewSeedSpec{Request: want}}
		if err := r.Create(ctx, s); err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
				before := p.DeepCopy()
				delete(p.Annotations, previewSeedCreated)
				if patchErr := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); patchErr != nil {
					return nil, patchErr
				}
			}
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if !equality.Semantic.DeepEqual(s.Spec.Request, want) || len(s.OwnerReferences) != 0 || !s.DeletionTimestamp.IsZero() || s.UID == "" {
		return nil, fmt.Errorf("seed request identity or immutable configuration conflict")
	}
	if identity := p.Annotations[previewSeedCreated]; identity != "" && identity != "true" && identity != string(s.UID) {
		return nil, fmt.Errorf("seed request UID changed; automatic replacement is forbidden")
	}
	if p.Annotations[previewSeedCreated] != string(s.UID) {
		before := p.DeepCopy()
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[previewSeedCreated] = string(s.UID)
		if err := r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// cancelSeed claims unclaimed requests with the same optimistic CAS required of
// executors. If the executor won Running first, only it may attest quiescence.
func cancelSeed(ctx context.Context, c client.Client, s *fleet.CelldPreviewSeed) (bool, error) {
	if seedTerminal(s) {
		return true, nil
	}
	if !s.Spec.Canceled {
		before := s.DeepCopy()
		s.Spec.Canceled = true
		if err := c.Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
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

func (r *PreviewReconciler) cancelPreviewSeed(ctx context.Context, p *fleet.CelldPreview) (bool, error) {
	if p.Spec.Seed == nil {
		return true, nil
	}
	s := &fleet.CelldPreviewSeed{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: seedName(p)}, s); err != nil {
		if apierrors.IsNotFound(err) && p.Annotations[previewSeedCreated] == "" {
			return true, nil
		}
		return false, err
	}
	if (p.Annotations[previewSeedCreated] != "" && p.Annotations[previewSeedCreated] != "true" && p.Annotations[previewSeedCreated] != string(s.UID)) || s.Spec.Request.Target.PreviewUID != string(p.UID) || s.Spec.Request.Target.PreviewName != p.Name || len(s.OwnerReferences) != 0 || !s.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("seed cancellation identity conflict")
	}
	return cancelSeed(ctx, r.Client, s)
}

func getFleetSeed(ctx context.Context, c client.Client, f *fleet.CelldFleet) (*fleet.CelldPreviewSeed, error) {
	ref := f.Spec.Storage.Initialization
	if ref == nil {
		return nil, fmt.Errorf("fleet has no initialization request")
	}
	s := &fleet.CelldPreviewSeed{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: ref.Name}, s); err != nil {
		return nil, err
	}
	target := s.Spec.Request.Target
	owner := metav1.GetControllerOf(f)
	if string(s.UID) != ref.UID || len(s.OwnerReferences) != 0 || !s.DeletionTimestamp.IsZero() || owner == nil || owner.Kind != "CelldPreview" || owner.APIVersion != fleet.GroupVersion.String() || string(owner.UID) != target.PreviewUID || owner.Name != target.PreviewName || target.FleetName != f.Name || target.StorageURL != f.Spec.Storage.URL() || f.Spec.Storage.PoolRef == nil || target.PoolRef != *f.Spec.Storage.PoolRef {
		return nil, fmt.Errorf("seed request does not bind this preview, fleet and storage identity")
	}
	return s, nil
}

func completedSeedReceipt(s *fleet.CelldPreviewSeed, f *fleet.CelldFleet) (string, error) {
	if s.Spec.Canceled || s.Status.Phase != "Succeeded" || s.Status.TargetFleetUID != string(f.UID) || s.Status.ExecutorID == "" || s.Status.Manifest == nil {
		return "", fmt.Errorf("seed has no successful completion for this fleet UID")
	}
	if err := s.Spec.Request.Selection.Validate(); err != nil {
		return "", err
	}
	expected := map[fleet.PreviewObjectReference]bool{}
	for _, o := range s.Spec.Request.Selection.Objects {
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
	return "ready:" + string(s.UID) + ":" + digest(b), nil
}

func seedReceiptReady(f *fleet.CelldFleet, res *fleet.CelldStorageReservation) bool {
	if f.Spec.Storage.Initialization == nil {
		return true
	}
	original := f.DeepCopy()
	if res.Spec.InitialReplicas > 0 {
		original.Spec.Replicas = res.Spec.InitialReplicas
	}
	if len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() || res.Spec != fleetReservationSpec(original) {
		return false
	}
	prefix := "ready:" + f.Spec.Storage.Initialization.UID + ":"
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
	if !time.Now().Before(s.Spec.Request.Deadline.Time) {
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
	s, err := getFleetSeed(ctx, r.Client, f)
	if err != nil {
		return true, err
	}
	stopped, err := cancelSeed(ctx, r.Client, s)
	if err != nil {
		return true, err
	}
	if !stopped {
		return true, fmt.Errorf("waiting for seed executor to stop all writes")
	}
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Spec: fleetReservationSpec(f)}
	if err := r.Create(ctx, res); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return true, err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(res), res); err != nil {
			return true, err
		}
	}
	// Existing initialized fleets must remain deletable even if the seed request
	// is later lost; deleteFleet checks the durable receipt before calling us.
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
