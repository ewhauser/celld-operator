package controller

import (
	"context"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func peerService(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *corev1.Service {
	t.Helper()
	s := &corev1.Service{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: f.Name + "-peers"}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

// legacyPeerService rewrites the live peer Service to what releases before #59
// created: identical except for an unset appProtocol, as the old operator wrote it.
func legacyPeerService(t *testing.T, r *Reconciler, f *fleet.CelldFleet, edit func(*corev1.Service)) *corev1.Service {
	t.Helper()
	s := peerService(t, r, f)
	s.Spec.Ports[0].AppProtocol = nil
	if edit != nil {
		edit(s)
	}
	if err := r.Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return peerService(t, r, f)
}

func appProtocol(s *corev1.Service) string {
	if p := s.Spec.Ports[0].AppProtocol; p != nil {
		return *p
	}
	return "<unset>"
}

func TestPeerServiceDeclaresTCPAppProtocol(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		f := fixture("alpha", "bucket-alpha", profile)
		r := setup(t, f)
		reason(t, reconcile(t, r, f), "Provisioning")
		s := peerService(t, r, f)
		// Meshes select protocol from appProtocol before the port name; without it
		// Istio sniffs 8081 as HTTP and corrupts the peer RPC byte stream.
		if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 8081 || appProtocol(s) != "tcp" || s.Spec.Ports[0].Name != "peer" {
			t.Fatalf("%s peer port does not declare TCP: %+v", profile, s.Spec.Ports)
		}
		app := &corev1.Service{}
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), app); err != nil {
			t.Fatal(err)
		}
		if app.Spec.Ports[0].AppProtocol != nil {
			t.Fatal("application Service protocol changed with the peer fix")
		}
	}
}

func TestLegacyPeerServiceUpgradesInPlace(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	r := setup(t, f)
	reason(t, reconcile(t, r, f), "Provisioning")
	reason(t, reconcile(t, r, f), "Provisioning")
	legacy := legacyPeerService(t, r, f, nil)
	// matches itself stays exact: other callers never treat the gap as a match.
	for _, obj := range prerequisites(f, r.Options) {
		if s, ok := obj.(*corev1.Service); ok && s.Name == legacy.Name && matches(s, legacy) {
			t.Fatal("exact comparison ignored a missing appProtocol")
		}
	}
	updates := 0
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.Service); ok {
			updates++
			if obj.GetResourceVersion() != legacy.ResourceVersion {
				t.Errorf("upgrade not pinned to the verified resourceVersion: %q", obj.GetResourceVersion())
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	reason(t, reconcile(t, r, f), "Provisioning")
	reason(t, reconcile(t, r, f), "Provisioning")
	got := peerService(t, r, f)
	if appProtocol(got) != "tcp" || got.UID != legacy.UID {
		t.Fatalf("legacy peer Service not upgraded in place: %s uid %s", appProtocol(got), got.UID)
	}
	if updates != 1 {
		t.Fatalf("want exactly one field upgrade, got %d updates", updates)
	}
}

func TestPrerequisiteUpgradeRefusesOtherDifferences(t *testing.T) {
	for name, edit := range map[string]func(*corev1.Service){
		"appProtocol-http": func(s *corev1.Service) { s.Spec.Ports[0].AppProtocol = new("http") },
		"appProtocol-mesh": func(s *corev1.Service) { s.Spec.Ports[0].AppProtocol = new("kubernetes.io/h2c") },
		"selector":         func(s *corev1.Service) { s.Spec.Selector["unexpected"] = "label" },
		"port-renamed":     func(s *corev1.Service) { s.Spec.Ports[0].Name = "tcp-peer" },
		"target-port":      func(s *corev1.Service) { s.Spec.Ports[0].TargetPort.IntVal = 9999 },
		"extra-port": func(s *corev1.Service) {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "extra", Port: 9090})
		},
		"not-ready-hidden": func(s *corev1.Service) { s.Spec.PublishNotReadyAddresses = false },
		"foreign-fleet":    func(s *corev1.Service) { s.Labels[FleetLabel] = "other-uid" },
		"owned": func(s *corev1.Service) {
			s.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "x"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "Bucket")
			r := setup(t, f)
			reconcile(t, r, f)
			// The appProtocol cases model another writer choosing a value: the field
			// is no longer unset, so it is a conflict, never an upgrade target.
			before := legacyPeerService(t, r, f, edit)
			reason(t, reconcile(t, r, f), "InfrastructureBlocked")
			after := peerService(t, r, f)
			if after.ResourceVersion != before.ResourceVersion || appProtocol(after) != appProtocol(before) {
				t.Fatalf("conflicting Service mutated: %s -> %s", appProtocol(before), appProtocol(after))
			}
		})
	}
}

// The operator's update is pinned to the resourceVersion it verified. A writer
// between that read and the update wins; the next reconcile re-verifies the
// writer's result from scratch and neither overwrites nor adopts it blindly.
func TestPrerequisiteUpgradeLosesConcurrentWriteSafely(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*corev1.Service)
		want string
	}{
		"unauthorized": {func(s *corev1.Service) { s.Spec.Selector["unexpected"] = "label" }, "InfrastructureBlocked"},
		"metadata-only": {func(s *corev1.Service) {
			s.Annotations = map[string]string{"example.com/note": "x"}
		}, "Provisioning"},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "Bucket")
			r := setup(t, f)
			reconcile(t, r, f)
			legacyPeerService(t, r, f, nil)
			base := r.Client
			raced := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Service); ok && !raced {
					raced = true
					other := &corev1.Service{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
						t.Fatal(err)
					}
					tc.edit(other)
					if err := c.Update(ctx, other); err != nil {
						t.Fatal(err)
					}
				}
				return c.Update(ctx, obj, opts...)
			}})
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			if !raced || !apierrors.IsConflict(err) {
				t.Fatalf("stale upgrade was not rejected: raced=%t err=%v", raced, err)
			}
			if s := peerService(t, r, f); appProtocol(s) != "<unset>" {
				t.Fatal("stale upgrade overwrote the concurrent writer")
			}
			r.Client = base
			reason(t, reconcile(t, r, f), tc.want)
			if s := peerService(t, r, f); (tc.want == "Provisioning") != (appProtocol(s) == "tcp") {
				t.Fatalf("re-verification after conflict: %s with appProtocol %s", tc.want, appProtocol(s))
			}
		})
	}
}

func TestPrerequisiteUpgradeWithoutRoleVerbNamesTheFix(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	reconcile(t, r, f)
	legacyPeerService(t, r, f, nil)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.Service); ok {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, obj.GetName(), nil)
		}
		return c.Update(ctx, obj, opts...)
	}})
	got := reconcile(t, r, f)
	reason(t, got, "InfrastructureBlocked")
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" && !strings.Contains(c.Message, "config/rbac/fleet-namespace.yaml") {
			t.Fatalf("condition does not name the Role: %q", c.Message)
		}
	}
}

// A real API server applies Service defaulting and resourceVersion preconditions
// the fake client only approximates.
func TestEnvtestLegacyPeerServiceUpgrade(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	f := x.fleet
	if s := peerService(t, r, f); appProtocol(s) != "tcp" {
		t.Fatalf("API server dropped appProtocol: %s", appProtocol(s))
	}
	legacy := legacyPeerService(t, r, f, nil)
	base := r.Client
	raced := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*corev1.Service); ok && !raced {
			raced = true
			other := &corev1.Service{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
				t.Fatal(err)
			}
			other.Annotations = map[string]string{"example.com/note": "x"}
			if err := c.Update(ctx, other); err != nil {
				t.Fatal(err)
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); !raced || !apierrors.IsConflict(err) {
		t.Fatalf("stale upgrade was not rejected by the API server: raced=%t err=%v", raced, err)
	}
	r.Client = base
	reason(t, reconcile(t, r, f), "Provisioning")
	got := peerService(t, r, f)
	if appProtocol(got) != "tcp" || got.UID != legacy.UID || got.Annotations["example.com/note"] != "x" {
		t.Fatalf("legacy peer Service not upgraded in place over the concurrent write: %s %+v", appProtocol(got), got.Annotations)
	}
	got.Spec.Ports[0].AppProtocol = new("http")
	if err := r.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, r, f), "InfrastructureBlocked")
	if s := peerService(t, r, f); appProtocol(s) != "http" {
		t.Fatal("another writer's appProtocol was overwritten")
	}
}
