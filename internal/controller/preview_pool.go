package controller

import (
	"context"
	"encoding/json"
	"fmt"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func bucketReservationName(bucket string) string { return "s3-" + digest([]byte(bucket))[:56] }

func storageEndpoint(s fleet.StorageSpec) string {
	if s.Endpoint != nil {
		return s.Endpoint.URL
	}
	return ""
}

func fleetReservationSpec(f *fleet.CelldFleet) fleet.ReservationSpec {
	return fleet.ReservationSpec{Initialization: f.Spec.Storage.Initialization.DeepCopy(), InitialReplicas: f.Spec.Replicas, Bucket: f.Spec.Storage.Bucket,
		Prefix: f.Spec.Storage.Prefix, Endpoint: storageEndpoint(f.Spec.Storage),
		FleetNamespace: f.Namespace, FleetName: f.Name, FleetUID: string(f.UID), SpecHash: specHash(f)}
}

// reservePreviewPool atomically claims the same root name used by a dedicated
// fleet. Independent child reservations can then claim only disjoint single
// segments. A list-before-create overlap check would race across controllers.
func reservePreviewPool(ctx context.Context, c client.Client, p *fleet.CelldFleet) error {
	b, err := json.Marshal(p.Spec.Previews)
	if err != nil {
		return err
	}
	want := fleet.ReservationSpec{OwnerKind: "FleetPreviews", Bucket: p.Spec.Previews.Storage.Bucket,
		FleetNamespace: p.Namespace, FleetName: p.Name, FleetUID: string(p.UID), SpecHash: digest(b)}
	if p.Spec.Previews.Storage.Endpoint != nil {
		want.Endpoint = p.Spec.Previews.Storage.Endpoint.URL
	}
	res := &fleet.CelldStorageReservation{Name: bucketReservationName(want.Bucket), Spec: want}
	if err := c.Create(ctx, res); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(res), res); err != nil {
			return err
		}
	}
	if !equality.Semantic.DeepEqual(want, res.Spec) || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() {
		return fmt.Errorf("bucket is already reserved to another pool or dedicated fleet identity")
	}
	return nil
}

// verifySharedStorage checks the permanent root before any child reservation
// or runtime is created. It deliberately needs no live pool object so deleting
// configuration cannot obstruct an existing fleet's safe lifecycle.
func verifySharedStorage(ctx context.Context, c client.Client, f *fleet.CelldFleet) error {
	ref := f.Spec.Storage.PreviewFleetRef
	if ref == nil {
		return nil
	}
	res := &fleet.CelldStorageReservation{}
	if err := c.Get(ctx, client.ObjectKey{Name: bucketReservationName(f.Spec.Storage.Bucket)}, res); err != nil {
		return fmt.Errorf("shared bucket reservation: %w", err)
	}
	s := res.Spec
	if s.OwnerKind != "FleetPreviews" || s.Prefix != "" || s.Bucket != f.Spec.Storage.Bucket || s.Endpoint != storageEndpoint(f.Spec.Storage) || s.FleetNamespace != f.Namespace || s.FleetName != ref.Name || s.FleetUID != ref.UID || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero() {
		return fmt.Errorf("shared bucket is not reserved to this pool UID, namespace and endpoint")
	}
	return nil
}
