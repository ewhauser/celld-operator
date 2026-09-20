package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

// Cluster-scoped API calls the reconciler makes must be granted by the
// ClusterRole in config/manager/operator.yaml. Unit tests run against the fake
// client, which never enforces RBAC, so a missing verb stays invisible until the
// operator is Forbidden in a real cluster. This test reads the shipped manifest
// and asserts the grants the reconciler depends on, including the cluster-wide
// Pod list EC2 fencing performs before terminating a whole instance
// (internal/controller/aws_fencing.go).
func TestClusterRoleGrantsTheVerbsTheReconcilerUses(t *testing.T) {
	role := clusterRole(t)
	for _, want := range []struct{ group, resource, verb string }{
		{"celld.eric.dev", "celldfleets", "list"},
		{"celld.eric.dev", "celldfleets", "patch"},
		{"celld.eric.dev", "celldstoragereservations", "update"},
		{"", "pods", "list"},
		{"", "persistentvolumes", "get"},
		{"", "nodes", "get"},
		{"", "nodes", "patch"},
		{"storage.k8s.io", "volumeattachments", "list"},
		{"storage.k8s.io", "storageclasses", "get"},
	} {
		if !grantedByClusterRole(role, want.group, want.resource, want.verb) {
			t.Errorf("ClusterRole does not grant %q on %s/%s; the reconciler would be Forbidden in a real cluster", want.verb, want.group, want.resource)
		}
	}
	// Pods are read cluster-wide only to prove node dedication before
	// TerminateInstances. Every Pod write stays in the per-namespace Role.
	for _, verb := range []string{"get", "watch", "create", "update", "patch", "delete", "deletecollection"} {
		if grantedByClusterRole(role, "", "pods", verb) {
			t.Errorf("ClusterRole grants %q on pods; only list belongs at cluster scope", verb)
		}
	}
	if grantedByClusterRole(role, "", "secrets", "get") {
		t.Error("ClusterRole grants Secret access that belongs to the per-namespace Role")
	}
}

func clusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "config", "manager", "operator.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		role := &rbacv1.ClusterRole{}
		if err := decoder.Decode(role); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if role.Kind == "ClusterRole" && role.Name == "celld-operator" {
			return role
		}
	}
	t.Fatal("config/manager/operator.yaml declares no celld-operator ClusterRole")
	return nil
}

func grantedByClusterRole(role *rbacv1.ClusterRole, group, resource, verb string) bool {
	return slices.ContainsFunc(role.Rules, func(rule rbacv1.PolicyRule) bool {
		return rbacMatches(rule.APIGroups, group) && rbacMatches(rule.Resources, resource) && rbacMatches(rule.Verbs, verb)
	})
}

func rbacMatches(values []string, want string) bool {
	return slices.Contains(values, want) || slices.Contains(values, "*")
}
