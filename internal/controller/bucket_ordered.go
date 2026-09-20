package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const bucketZoneGate = "celld.eric.dev/bucket-zone"

func orderedBucket(f *fleet.CelldFleet) bool {
	return f.Spec.Profile == "Bucket" && f.Spec.BucketWorkload == "Ordered"
}
func bucketOrdinal(f *fleet.CelldFleet, name string) (int, error) {
	raw, ok := strings.CutPrefix(name, f.Name+"-")
	n, err := strconv.Atoi(raw)
	if !ok || err != nil || n < 0 || strconv.Itoa(n) != raw {
		return 0, errors.New("invalid Bucket ordinal")
	}
	return n, nil
}

// Assign before scheduling. StatefulSet scale-down removes the highest ordinal;
// modulo assignment leaves a balanced prefix with every configured AZ represented.
// No pod deletion-cost hint or scheduler-dependent victim choice is involved.
func (r *Reconciler) scheduleOrderedBucket(ctx context.Context, f *fleet.CelldFleet, j *fleetState) error {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f))); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !slices.ContainsFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == bucketZoneGate }) {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		n, err := bucketOrdinal(f, pod.Name)
		if err != nil || owner == nil || owner.UID != j.WorkloadUID || owner.Kind != "StatefulSet" || owner.APIVersion != "apps/v1" || owner.Name != f.Name || pod.Spec.NodeName != "" || !pod.DeletionTimestamp.IsZero() {
			return errors.New("gated Bucket pod identity changed")
		}
		if f.Spec.Placement.Mode != "Relaxed" {
			zone := f.Spec.Placement.Zones[n%len(f.Spec.Placement.Zones)]
			if pod.Spec.NodeSelector == nil {
				pod.Spec.NodeSelector = map[string]string{}
			}
			if old := pod.Spec.NodeSelector[corev1.LabelTopologyZone]; old != "" && old != zone {
				return fmt.Errorf("bucket ordinal %d has conflicting zone", n)
			}
			pod.Spec.NodeSelector[corev1.LabelTopologyZone] = zone
		}
		pod.Spec.SchedulingGates = slices.DeleteFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == bucketZoneGate })
		// ResourceVersion protects against concurrent admission or an old issuer.
		if err := r.Update(ctx, pod); err != nil {
			return err
		}
	}
	return nil
}
