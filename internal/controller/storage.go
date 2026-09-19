package controller

import (
	"context"
	"fmt"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// initialClaims binds each deterministic ordinal name by atomic Create before
// the StatefulSet can consume it. No existing claim is trusted based on labels
// alone, even if its labels happen to match this fleet. The creation journal
// blocks retries after a crash or a partial allocation, preserving every claim.
func initialClaims(f *fleet.CelldFleet, workload client.Object) []*corev1.PersistentVolumeClaim {
	sts, ok := workload.(*appsv1.StatefulSet)
	if !ok || f.Spec.Profile != "PersistentFleet" {
		return nil
	}
	claims := make([]*corev1.PersistentVolumeClaim, 0, f.Spec.Replicas)
	for ordinal := int32(0); ordinal < f.Spec.Replicas; ordinal++ {
		claim := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
		claim.ObjectMeta = metadata(f, fmt.Sprintf("data-%s-%d", f.Name, ordinal))
		claim.Annotations = map[string]string{"celld.eric.dev/storage-reservation": reservationName(f)}
		claims = append(claims, claim)
	}
	return claims
}

func (r *Reconciler) checkInitialClaims(ctx context.Context, claims []*corev1.PersistentVolumeClaim) error {
	for _, claim := range claims {
		existing := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, client.ObjectKeyFromObject(claim), existing)
		if err == nil {
			return fmt.Errorf("PVC %s already exists; initial provisioning cannot verify retained disk identity or adopt existing claims", claim.Name)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("verify PVC %s: %w", claim.Name, err)
		}
	}
	return nil
}
