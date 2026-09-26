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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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
// and asserts the grants the current-operation reconciler depends on.
func TestClusterRoleGrantsTheVerbsTheReconcilerUses(t *testing.T) {
	role := clusterRole(t)
	for _, want := range []struct{ group, resource, verb string }{
		{"celld.eric.dev", "celldfleets", "list"},
		{"celld.eric.dev", "celldfleets", "patch"},
		{"celld.eric.dev", "celldstoragereservations", "update"},
		{"storage.k8s.io", "storageclasses", "get"},
	} {
		if !grantedByClusterRole(role, want.group, want.resource, want.verb) {
			t.Errorf("ClusterRole does not grant %q on %s/%s; the reconciler would be Forbidden in a real cluster", want.verb, want.group, want.resource)
		}
	}
	// The operator does not track volumes or nodes (ADR 0024).
	for _, unused := range []struct{ group, resource string }{{"", "persistentvolumes"}, {"", "nodes"}, {"storage.k8s.io", "volumeattachments"}} {
		if grantedByClusterRole(role, unused.group, unused.resource, "get") || grantedByClusterRole(role, unused.group, unused.resource, "list") {
			t.Errorf("ClusterRole grants %s/%s, which the operator no longer reads", unused.group, unused.resource)
		}
	}
	// No Pod access belongs at cluster scope after removal of EC2 fencing.
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
		if grantedByClusterRole(role, "", "pods", verb) {
			t.Errorf("ClusterRole grants %q on pods; pod access belongs in the fleet namespace", verb)
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

// Audit actual client calls, including subresources and request namespace. Keep
// fixture mutations outside this wrapper: they model users and kubelet, whose
// permissions are deliberately broader than the operator's.
type manifestClient struct {
	client.Client
	t                  *testing.T
	cluster, namespace []rbacv1.PolicyRule
	seen               map[string]bool
}

func auditManifestClient(t *testing.T, c client.Client) *manifestClient {
	t.Helper()
	data, err := os.ReadFile("../../config/rbac/fleet-namespace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.Role
	if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&role); err != nil {
		t.Fatal(err)
	}
	if role.Kind != "Role" {
		t.Fatal("namespace manifest has no Role")
	}
	return &manifestClient{Client: c, t: t, cluster: clusterRole(t).Rules, namespace: role.Rules, seen: map[string]bool{}}
}

func (c *manifestClient) check(obj runtime.Object, namespace, verb, subresource string) {
	c.t.Helper()
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		c.t.Fatal(err)
	}
	gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
	resource, _ := meta.UnsafeGuessKindToResource(gvk)
	name := resource.Resource
	if subresource != "" {
		name += "/" + subresource
	}
	c.seen[gvk.Group+"/"+name+"/"+verb+"/"+namespace] = true
	rules := append([]rbacv1.PolicyRule{}, c.cluster...)
	if namespace != "" {
		rules = append(rules, c.namespace...)
	}
	if !grantedByClusterRole(&rbacv1.ClusterRole{Rules: rules}, gvk.Group, name, verb) {
		c.t.Errorf("issued API call missing from shipped RBAC: %s %s/%s namespace=%q", verb, gvk.Group, name, namespace)
	}
}
func (c *manifestClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.check(obj, key.Namespace, "get", "")
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *manifestClient) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	o := (&client.ListOptions{}).ApplyOptions(opts)
	c.check(obj, o.Namespace, "list", "")
	return c.Client.List(ctx, obj, opts...)
}
func (c *manifestClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.check(obj, obj.GetNamespace(), "create", "")
	return c.Client.Create(ctx, obj, opts...)
}
func (c *manifestClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.check(obj, obj.GetNamespace(), "update", "")
	return c.Client.Update(ctx, obj, opts...)
}
func (c *manifestClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.check(obj, obj.GetNamespace(), "patch", "")
	return c.Client.Patch(ctx, obj, patch, opts...)
}
func (c *manifestClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.check(obj, obj.GetNamespace(), "delete", "")
	return c.Client.Delete(ctx, obj, opts...)
}
func (c *manifestClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	o := (&client.DeleteAllOfOptions{}).ApplyOptions(opts)
	c.check(obj, o.Namespace, "deletecollection", "")
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}
func (c *manifestClient) Status() client.SubResourceWriter { return c.SubResource("status") }
func (c *manifestClient) SubResource(name string) client.SubResourceClient {
	return &manifestSubresource{SubResourceClient: c.Client.SubResource(name), parent: c, name: name}
}

type manifestSubresource struct {
	client.SubResourceClient
	parent *manifestClient
	name   string
}

func (c *manifestSubresource) Get(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceGetOption) error {
	c.parent.check(obj, obj.GetNamespace(), "get", c.name)
	return c.SubResourceClient.Get(ctx, obj, sub, opts...)
}
func (c *manifestSubresource) Create(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceCreateOption) error {
	c.parent.check(obj, obj.GetNamespace(), "create", c.name)
	return c.SubResourceClient.Create(ctx, obj, sub, opts...)
}
func (c *manifestSubresource) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	c.parent.check(obj, obj.GetNamespace(), "update", c.name)
	return c.SubResourceClient.Update(ctx, obj, opts...)
}
func (c *manifestSubresource) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	c.parent.check(obj, obj.GetNamespace(), "patch", c.name)
	return c.SubResourceClient.Patch(ctx, obj, patch, opts...)
}
