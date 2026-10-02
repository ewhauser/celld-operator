package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Recovery can win after the Pod is held for replacement but before the
// guarded Pod deletion commits. That conflict must leave its disk untouched,
// and retry must release the uncommitted hold rather than delete the disk.
func TestReliabilityPersistentRecoveryBeforePodDeletion(t *testing.T) {
	x := agedFleet(t)
	x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
	oldPod := x.pod("alpha-1")
	claim := x.claim("data-alpha-1")
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if err := x.r.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	oldClaimUID := claim.UID
	base := x.r.Client
	injected := false
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if p, pod := obj.(*corev1.Pod); pod && p.Name == "alpha-1" && !injected {
				current := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(oldPod), current); err != nil {
					return err
				}
				current.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(x.clock)}}
				if err := c.Status().Update(ctx, current); err != nil {
					return err
				}
				injected = true
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	_, _, _, firstErr := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
	x.r.Client = base
	if !injected || !apierrors.IsConflict(firstErr) {
		t.Fatalf("recovery did not conflict with guarded Pod deletion: recovered=%v, err=%v", injected, firstErr)
	}
	currentClaim := x.claim("data-alpha-1")
	claimTerminating := !currentClaim.DeletionTimestamp.IsZero()
	currentPod := x.pod("alpha-1")
	healthyAfterFirst := currentPod.UID == oldPod.UID && podReady(currentPod)
	_, _, _, nextErr := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
	_, podRetained := x.podUIDs()["alpha-1"]
	if nextErr != nil {
		t.Fatal(nextErr)
	}
	finalClaim := x.claim("data-alpha-1")
	if finalClaim.UID != oldClaimUID || !finalClaim.DeletionTimestamp.IsZero() || claimTerminating || !healthyAfterFirst || !podRetained {
		t.Fatalf("recovery before guarded deletion lost storage: PVC deletion committed=%v, healthy Pod retained after retry=%v", claimTerminating, podRetained)
	}
	if p := x.pod("alpha-1"); p.Annotations[replacementAnnotation] != "" || slices.Contains(p.Finalizers, replacementFinalizer) {
		t.Fatal("retry did not release the uncommitted replacement hold")
	}
}
