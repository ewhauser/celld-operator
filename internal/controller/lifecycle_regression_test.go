package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestConcurrentLossFencesReplicaIssuance(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "loss journaled", true: "crash before loss journal"}[crash], func(t *testing.T) {
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
			base := r.Client
			injected := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if w, ok := obj.(*appsv1.StatefulSet); ok && *w.Spec.Replicas == 2 && !injected {
					injected = true
					bad := *e
					bad.loss = true
					other := &Reconciler{Client: c, Options: r.Options, NetworkPolicyEnforced: true, localLifecycle: &bad}
					if crash {
						other.Client = interceptor.NewClient(c, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							if _, ok := obj.(*fleet.CelldStorageReservation); ok {
								return errors.New("crash before journal write")
							}
							return c.Update(ctx, obj, opts...)
						}})
					}
					_, err := other.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
					if !crash && err != nil {
						return err
					}
				}
				return c.Update(ctx, obj, opts...)
			}})
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			if err != nil && !apierrors.IsConflict(err) {
				t.Fatal(err)
			}
			w := &appsv1.StatefulSet{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if *w.Spec.Replicas != 3 {
				t.Fatalf("removed replica after concurrent loss: %d", *w.Spec.Replicas)
			}
			r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true, localLifecycle: e}
			for range 4 {
				reconcile(t, r, f)
			}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if *w.Spec.Replicas != 3 || getJournal(t, r, f).Loss == "" {
				t.Fatal("restart forgot loss or rearmed removal")
			}
		})
	}
}

func TestReplicaChangeBeforeInitialWorkload(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", profile)
			r := setup(t, f)
			base := r.Client
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("transient API failure")
				}
				return c.Create(ctx, obj, opts...)
			}})
			reason(t, reconcile(t, r, f), "InfrastructureBlocked")
			r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true}
			f = desiredCount(t, r, f, 4)
			reason(t, reconcile(t, r, f), "Provisioning")
			reason(t, reconcile(t, r, f), "Provisioning")
			j := getJournal(t, r, f)
			if j.Initial != 3 || j.Applied != 4 {
				t.Fatalf("lost original reservation baseline: %+v", j)
			}
			f = desiredCount(t, r, f, 5)
			for range 5 {
				reconcile(t, r, f)
			}
			if getJournal(t, r, f).Applied != 5 {
				t.Fatal("subsequent expansion failed")
			}
			current := &fleet.CelldFleet{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), current); err != nil {
				t.Fatal(err)
			}
			current.Spec.Storage.SizeGiB++
			if err := r.Update(t.Context(), current); err != nil {
				t.Fatal(err)
			}
			reason(t, reconcile(t, r, current), "StorageScopeConflict")
		})
	}
}

func TestLossFencePreservesAlreadyIssuedRemoval(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.Options.LocalTest = true
	r.localLifecycle = &localEvidence{now: time.Now(), stopped: true}
	reconcile(t, r, f)
	reconcile(t, r, f)
	f = desiredCount(t, r, f, 2)
	reconcile(t, r, f)
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(res)
	if err != nil {
		t.Fatal(err)
	}
	w := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	stale := w.DeepCopy()
	if err := r.applyReplicas(t.Context(), w, j.Operation); err != nil {
		t.Fatal(err)
	}
	// The fence must retry a benign resource-version conflict without restoring
	// the stale count, erasing other annotations, or reissuing the removal.
	base := r.Client
	once := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if w, ok := obj.(*appsv1.StatefulSet); ok && !once {
			once = true
			other := &appsv1.StatefulSet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(w), other); err != nil {
				return err
			}
			other.Annotations["example.com/concurrent"] = "keep"
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.recordLoss(t.Context(), f, stale, res, j, "possible loss"); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if *w.Spec.Replicas != 2 || w.Annotations[lossFenceKey] == "" || w.Annotations["example.com/concurrent"] != "keep" {
		t.Fatalf("loss fence overwrote a concurrent update: %+v", w)
	}
	r.Client = base
	for range 3 {
		reconcile(t, r, f)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	if *w.Spec.Replicas != 2 || getJournal(t, r, f).Loss == "" {
		t.Fatal("loss fence did not survive reconciliation")
	}
}
