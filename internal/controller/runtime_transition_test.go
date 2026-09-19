package controller

import (
	"context"
	"errors"
	"testing"

	v041 "github.com/ewhauser/celld-operator/internal/runtime/v041"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestStoppedImageCASLostResponseAndStaleIssuer(t *testing.T) {
	p, _ := coordinatedSetup(t)
	p.j.RuntimeImage = v041.Image
	p.j.Maintenance = &maintenanceOperation{ID: "upgrade", Kind: "Upgrade", Phase: "Empty", Coordinated: true, SourceImage: v041.Image, TargetImage: Image, TargetReplicas: 2}
	source := p.f.DeepCopy()
	source.Spec.RuntimeImage = v041.Image
	p.w.Spec.Template = workload(source, p.r.Options).(*appsv1.StatefulSet).Spec.Template
	setReplicas(p.w, 0)
	p.w.Annotations = map[string]string{operationKey: "upgrade"}
	if err := p.r.Update(t.Context(), p.w); err != nil {
		t.Fatal(err)
	}
	stale := p.w.DeepCopy()
	base := p.r.Client
	p.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if err := c.Update(ctx, obj, opts...); err != nil {
			return err
		}
		return errors.New("lost response after image CAS")
	}})
	if err := p.r.installStoppedRuntime(t.Context(), source, p.j, p.w); err == nil {
		t.Fatal("missing injected failure")
	}
	p.r.Client = base
	if err := p.r.Get(t.Context(), client.ObjectKeyFromObject(p.w), p.w); err != nil {
		t.Fatal(err)
	}
	if err := p.r.installStoppedRuntime(t.Context(), source, p.j, p.w); err != nil {
		t.Fatal("cannot recover applied CAS", err)
	}
	if p.w.Spec.Template.Spec.Containers[0].Image != Image || replicas(p.w) != 0 || p.j.RuntimeImage != v041.Image {
		t.Fatal("premature resume or image promotion")
	}
	if err := p.r.installStoppedRuntime(t.Context(), source, p.j, stale); err == nil {
		t.Fatal("stale source issuer overwrote target")
	}
	p.w.Annotations[runtimeTransitionKey] = "other-operation"
	if _, err := transitionWorkloadImage(p.j, p.w); err == nil {
		t.Fatal("accepted target installed by another authority")
	}
}

func TestVersionTransitionRejectsUnqualifiedAuthority(t *testing.T) {
	for _, scenario := range []string{"rollback", "same-image", "wrong-source", "not-coordinated", "wrong-phase", "missing-target"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := coordinatedSetup(t)
			p.j.RuntimeImage = v041.Image
			m := &maintenanceOperation{ID: "upgrade", Kind: "Upgrade", Phase: "Capture", Coordinated: true, SourceImage: v041.Image, TargetImage: Image, TargetReplicas: 2}
			p.j.Maintenance = m
			switch scenario {
			case "rollback":
				p.j.RuntimeImage = Image
				m.SourceImage = Image
				m.TargetImage = v041.Image
			case "same-image":
				m.TargetImage = v041.Image
			case "wrong-source":
				m.SourceImage = Image
			case "not-coordinated":
				m.Coordinated = false
			case "wrong-phase":
				m.Phase = "Recovering"
			case "missing-target":
				m.TargetImage = ""
			}
			if validateMaintenanceJournal(p.j) == nil {
				t.Fatal("accepted invalid transition")
			}
		})
	}
}

func TestRequestedVersionDoesNotChangeInFlightEvidence(t *testing.T) {
	p, _ := coordinatedSetup(t)
	p.f.Spec.RuntimeImage = v041.Image // Unsupported rollback request while old work finishes.
	if _, err := p.r.persistentMembers(t.Context(), p.f, p.j, 2, false); err != nil {
		t.Fatal("request poisoned applied-runtime evidence", err)
	}
	p.j.RuntimeImage = v041.Image
	p.j.Maintenance = &maintenanceOperation{Kind: "Upgrade", Phase: "Resuming", TargetImage: Image}
	if _, err := p.r.persistentMembers(t.Context(), p.f, p.j, 2, false); err != nil {
		t.Fatal("installed target not selected during resume", err)
	}
}
