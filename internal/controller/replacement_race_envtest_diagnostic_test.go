package controller

import (
	"context"
	"fmt"
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

// The API server assigns creation timestamps, so this counterpart isolates an
// already-selected replacement by calling replaceMember with real observations.
// The fake diagnostic covers eligibility; this diagnostic covers actual PVC
// deletion, Pod resourceVersion enforcement, and healMembers' subsequent action.
// It deliberately asserts the unresolved invariant only when explicitly opted in.
func TestEnvtestDiagnosticPersistentReplacementRecoveryAfterFinalRead(t *testing.T) {
	if os.Getenv("CELLD_TEST_REPLACEMENT_RACE") != "1" {
		t.Skip("known unresolved Pod/PVC replacement race; set CELLD_TEST_REPLACEMENT_RACE=1 and KUBEBUILDER_ASSETS to reproduce it against kube-apiserver")
	}
	r, x := envtestSetup(t, "PersistentFleet")
	x.provision(t, r)
	sts := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), sts); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	now := time.Now().UTC()
	var observedPod *corev1.Pod
	var observedClaim *corev1.PersistentVolumeClaim
	for ordinal := range replicas(sts) {
		claimTemplate := sts.Spec.VolumeClaimTemplates[0]
		pvc := &corev1.PersistentVolumeClaim{
			Name: claimName(x.fleet, ordinal), Namespace: x.namespace,
			Labels: labels(x.fleet), Annotations: claimTemplate.Annotations,
			Finalizers: []string{"kubernetes.io/pvc-protection"}, Spec: claimTemplate.Spec,
		}
		if err := r.Create(t.Context(), pvc); err != nil {
			t.Fatal(err)
		}
		pvc.Status.Phase = corev1.ClaimBound
		if err := r.Status().Update(t.Context(), pvc); err != nil {
			t.Fatal(err)
		}
		spec := sts.Spec.Template.Spec.DeepCopy()
		spec.NodeName = fmt.Sprintf("diagnostic-node-%d", ordinal)
		// There is no kubelet in envtest. Zero grace lets the real API server
		// remove the Pod immediately if the next heal asks to delete it.
		spec.TerminationGracePeriodSeconds = new(int64(0))
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}})
		p := &corev1.Pod{
			Name: memberName(x.fleet, ordinal), Namespace: x.namespace, Labels: labels(x.fleet),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: new(true)}},
			Spec:            *spec,
		}
		if err := r.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		ready := corev1.ConditionTrue
		if ordinal == 1 {
			ready = corev1.ConditionFalse
		}
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}}}
		if err := r.Status().Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if ordinal == 1 {
			observedPod, observedClaim = p.DeepCopy(), pvc.DeepCopy()
		}
	}
	if observedPod == nil || observedClaim == nil {
		t.Fatal("diagnostic requires a fleet with member ordinal 1")
	}
	readSeen, injected := false, false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if p, pod := obj.(*corev1.Pod); pod && key.Name == observedPod.Name {
				if p.UID != observedPod.UID || podReady(p) {
					t.Fatal("final Pod read did not observe the unchanged down member")
				}
				readSeen = true
			}
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if pvc, claim := obj.(*corev1.PersistentVolumeClaim); claim && pvc.Name == observedClaim.Name && !injected {
				if !readSeen {
					t.Fatal("replacement attempted claim deletion before the final Pod read")
				}
				current := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(observedPod), current); err != nil {
					return err
				}
				current.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
				if err := c.Status().Update(ctx, current); err != nil {
					return err
				}
				injected = true
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	firstErr := r.replaceMember(t.Context(), observedClaim, observedPod)
	r.Client = base
	if !readSeen || !injected || firstErr != nil && !apierrors.IsConflict(firstErr) {
		t.Fatalf("diagnostic did not reproduce the intended window: finalRead=%v, recovered=%v, err=%v", readSeen, injected, firstErr)
	}
	currentClaim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(observedClaim), currentClaim); err != nil {
		t.Fatal(err)
	}
	currentPod := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(observedPod), currentPod); err != nil {
		t.Fatal(err)
	}
	claimTerminating := !currentClaim.DeletionTimestamp.IsZero()
	healthyAfterFirst := currentPod.UID == observedPod.UID && currentPod.DeletionTimestamp.IsZero() && podReady(currentPod)
	t.Logf("real API first replacement: Pod delete conflict=%v; observed Pod RV=%s; recovered Pod RV=%s; PVC deletionTimestamp=%s; recovered Pod retained=%v", firstErr, observedPod.ResourceVersion, currentPod.ResourceVersion, currentClaim.DeletionTimestamp, healthyAfterFirst)

	action, _, _, nextErr := r.healMembers(t.Context(), x.fleet, sts, true)
	if nextErr != nil {
		t.Fatal(nextErr)
	}
	nextPod := &corev1.Pod{}
	getErr := r.Get(t.Context(), client.ObjectKeyFromObject(observedPod), nextPod)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		t.Fatal(getErr)
	}
	podRetained := getErr == nil && nextPod.UID == observedPod.UID && nextPod.DeletionTimestamp.IsZero() && podReady(nextPod)
	t.Logf("real API next heal: action=%q; recovered Pod Get=%v; recovered Pod retained=%v", action, getErr, podRetained)
	finalClaim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(observedClaim), finalClaim); err != nil {
		t.Fatal(err)
	}
	if finalClaim.UID != observedClaim.UID || !finalClaim.DeletionTimestamp.IsZero() || claimTerminating || !healthyAfterFirst || !podRetained {
		t.Fatalf("desired invariant violated against kube-apiserver: readiness recovered after the final read, but retained PVC deletion committed=%v and healthy Pod retained after retry=%v", claimTerminating, podRetained)
	}
}
