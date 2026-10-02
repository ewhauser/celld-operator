package controller

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// This diagnostic asserts the desired safety invariant across a remaining
// multi-object race. It intentionally fails until replacement can coordinate
// Pod recovery and PVC deletion beyond the final Pod read. Keep this separate
// from the passing reliability regressions so normal checks remain useful.
func TestDiagnosticPersistentReplacementRecoveryAfterFinalRead(t *testing.T) {
	if os.Getenv("CELLD_TEST_REPLACEMENT_RACE") != "1" {
		t.Skip("known unresolved Pod/PVC replacement race; set CELLD_TEST_REPLACEMENT_RACE=1 to reproduce recovery after the final Pod read and before PVC deletion")
	}
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
	readSeen, injected := false, false
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if p, pod := obj.(*corev1.Pod); pod && key.Name == "alpha-1" {
				if p.UID != oldPod.UID || podReady(p) {
					t.Fatal("diagnostic requires the final Pod read to observe the unchanged down member")
				}
				readSeen = true
			}
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if pvc, claim := obj.(*corev1.PersistentVolumeClaim); claim && pvc.Name == "data-alpha-1" && !injected {
				if !readSeen {
					t.Fatal("replacement attempted claim deletion before its final Pod read")
				}
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
	if !readSeen || !injected || firstErr != nil && !apierrors.IsConflict(firstErr) {
		t.Fatalf("diagnostic did not reproduce the intended window: finalRead=%v, recovered=%v, err=%v", readSeen, injected, firstErr)
	}
	currentClaim := x.claim("data-alpha-1")
	claimTerminating := !currentClaim.DeletionTimestamp.IsZero()
	currentPod := x.pod("alpha-1")
	healthyAfterFirst := currentPod.UID == oldPod.UID && podReady(currentPod)
	t.Logf("first heal: Pod delete conflict=%v; PVC terminating=%v; recovered Pod retained=%v", firstErr, claimTerminating, healthyAfterFirst)

	action, _, _, nextErr := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
	_, podRetained := x.podUIDs()["alpha-1"]
	t.Logf("next heal: action=%q; err=%v; recovered Pod retained=%v", action, nextErr, podRetained)
	if nextErr != nil {
		t.Fatal(nextErr)
	}
	finalClaim := x.claim("data-alpha-1")
	if finalClaim.UID != oldClaimUID || !finalClaim.DeletionTimestamp.IsZero() || claimTerminating || !healthyAfterFirst || !podRetained {
		t.Fatalf("desired invariant violated: readiness recovered after the final read, but retained PVC deletion committed=%v and healthy Pod retained after retry=%v", claimTerminating, podRetained)
	}
}
