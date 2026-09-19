package controller

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A fleet namespace without the namespaced Role must surface as a condition
// naming the fix, with no namespace resource created and no silent requeue loop.
func TestFleetNamespaceWithoutRoleIsReportedNotReconciled(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	base := r.Client
	touched := 0
	forbidden := func(obj client.Object) error {
		if obj.GetNamespace() != f.Namespace {
			return nil
		}
		if _, isFleet := obj.(*fleet.CelldFleet); isFleet {
			return nil
		}
		touched++
		gvk := obj.GetObjectKind().GroupVersionKind()
		return apierrors.NewForbidden(schema.GroupResource{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind)}, obj.GetName(), nil)
	}
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			// Every namespaced read is forbidden, including the workload read the
			// status reporter performs; the condition must still be written.
			if _, isFleet := obj.(*fleet.CelldFleet); !isFleet && key.Namespace == f.Namespace {
				touched++
				return apierrors.NewForbidden(schema.GroupResource{Resource: strings.ToLower(obj.GetObjectKind().GroupVersionKind().Kind)}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := forbidden(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	for range 3 {
		got := reconcile(t, r, f)
		reason(t, got, "NamespaceAccessDenied")
		var msg string
		for _, c := range got.Status.Conditions {
			if c.Type == "Ready" {
				msg = c.Message
			}
		}
		if !strings.Contains(msg, "config/rbac/fleet-namespace.yaml") || !strings.Contains(msg, f.Namespace) {
			t.Fatalf("condition does not name the remedy: %q", msg)
		}
	}
	deployments := &appsv1.DeploymentList{}
	if err := base.List(t.Context(), deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 0 {
		t.Fatal("workload created without namespace authority")
	}
	if touched == 0 {
		t.Fatal("test never exercised a forbidden namespaced call")
	}
	// Reservations are cluster-scoped and permitted, but nothing in the namespace
	// may be provisioned before the Role exists.
	services := &corev1.ServiceList{}
	if err := base.List(t.Context(), services); err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 0 {
		t.Fatal("prerequisites created without namespace authority")
	}
}
