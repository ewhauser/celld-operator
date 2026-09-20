package controller

import (
	"context"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	crreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func TestMaintenancePreservesServingReadiness(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, kind := range []string{"pause", "restart", "upgrade"} {
			t.Run(profile+kind, func(t *testing.T) {
				f := fixture("alpha", "bucket-alpha", profile)
				r := setup(t, f)
				f = reconcile(t, r, f)
				f = reconcile(t, r, f)
				w := emptyObject(workload(f, r.Options))
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
					t.Fatal(err)
				}
				switch w := w.(type) {
				case *appsv1.Deployment:
					w.Status.ReadyReplicas = 3
					w.Status.ObservedGeneration = w.Generation
				case *appsv1.StatefulSet:
					w.Status.ReadyReplicas = 3
					w.Status.ObservedGeneration = w.Generation
				}
				if err := r.Status().Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
				f = reconcile(t, r, f)
				switch kind {
				case "pause":
					f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
				case "restart":
					f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "one"}
				case "upgrade":
					f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
				if err := r.Update(t.Context(), f); err != nil {
					t.Fatal(err)
				}
				f = reconcile(t, r, f)
				if f.Status.ReadyReplicas != 3 || !meta.IsStatusConditionTrue(f.Status.Conditions, "Ready") {
					t.Fatalf("maintenance erased serving readiness: %+v", f.Status)
				}
				if !meta.IsStatusConditionTrue(f.Status.Conditions, "Blocked") {
					t.Fatal("lost blocker")
				}
			})
		}
	}
}
func TestStrictPlacementSeparatesHosts(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		f := fixture("alpha", "bucket-alpha", profile)
		p := podTemplate(f, Options{}).Spec
		if p.Affinity.PodAntiAffinity == nil || len(p.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
			t.Fatalf("%s lacks node separation", profile)
		}
		term := p.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
		if term.TopologyKey != corev1.LabelHostname || term.LabelSelector.MatchLabels[FleetLabel] != string(f.UID) {
			t.Fatal("wrong identity/domain")
		}
		f.Spec.Placement.Mode = "Relaxed"
		p = podTemplate(f, Options{}).Spec
		if len(p.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 || len(p.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
			t.Fatal("relaxation must prefer node separation")
		}
	}
}
func TestSlowFleetDoesNotBlockOtherFleet(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	fast := make(chan struct{}, 1)
	options := fleetControllerOptions()
	options.Reconciler = crreconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
		if req.Name == "slow" {
			close(entered)
			<-ctx.Done()
		} else {
			fast <- struct{}{}
		}
		return ctrl.Result{}, nil
	})
	// Controller names are process-global, so a fixed name fails on the second
	// -count iteration; let the runtime skip the uniqueness check.
	options.SkipNameValidation = new(true)
	c, err := crcontroller.NewUnmanaged("review-fairness", options)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan event.GenericEvent, 2)
	if err = c.Watch(source.Channel(events, &handler.EnqueueRequestForObject{})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	events <- event.GenericEvent{Object: &fleet.CelldFleet{Name: "slow"}}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("slow fleet did not start")
	}
	events <- event.GenericEvent{Object: &fleet.CelldFleet{Name: "fast"}}
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("slow fleet blocks other fleet")
	}
}
