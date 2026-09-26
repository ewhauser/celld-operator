package controller

import (
	"fmt"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// initialClaims binds each deterministic ordinal name by atomic Create before
// the StatefulSet can consume it. No existing claim is trusted based on labels
// alone, even if its labels happen to match this fleet. The creation intent
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

// launcherGate is the scheduling gate of Pods created from templates written
// before ADR 0023; releaseLauncherGates admits them.
const launcherGate = "celld.eric.dev/exclusive-volume"

func persistentAccessModes(opts Options) []corev1.PersistentVolumeAccessMode {
	if !opts.LocalTest || opts.LocalRWOP {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	}
	return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
}

func (r *Reconciler) supportedCSI(driver string) bool {
	return driver == "ebs.csi.aws.com" || (r.Options.LocalTest && driver == localCSIDriver)
}
