package controller

import (
	"context"
	"slices"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func meshFixture(profile string) *fleet.CelldFleet {
	f := fixture("alpha", "bucket-alpha", profile)
	f.Spec.Mesh = &fleet.MeshSpec{}
	f.Default()
	return f
}

func authorizationPolicy(t *testing.T, r *Reconciler, f *fleet.CelldFleet) (*unstructured.Unstructured, error) {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(authorizationPolicyGVK)
	err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: f.Name + "-mesh"}, obj)
	return obj, err
}

func policyRules(t *testing.T, p *unstructured.Unstructured) []map[string]any {
	t.Helper()
	items, _, err := unstructured.NestedSlice(p.Object, "spec", "rules")
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for _, item := range items {
		out = append(out, item.(map[string]any))
	}
	return out
}

func rulePorts(rule map[string]any) []any {
	to := rule["to"].([]any)
	return to[0].(map[string]any)["operation"].(map[string]any)["ports"].([]any)
}

func istiodEgress(p *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
	for i, rule := range p.Spec.Egress {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntVal == istiodXDSPort {
			return &p.Spec.Egress[i]
		}
	}
	return nil
}

// Existing fleets carry no mesh block and keep exactly what they had.
func TestMeshOffRendersNothingIstio(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	reason(t, reconcile(t, r, f), "Provisioning")
	template := podTemplate(f, r.Options)
	if _, ok := template.Labels[istioInjectLabel]; ok || len(template.Annotations) != 0 {
		t.Fatalf("mesh metadata on a fleet outside the mesh: %v %v", template.Labels, template.Annotations)
	}
	p := &networkingv1.NetworkPolicy{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
		t.Fatal(err)
	}
	if istiodEgress(p) != nil {
		t.Fatal("istiod egress opened for a fleet outside the mesh")
	}
	if _, err := authorizationPolicy(t, r, f); !apierrors.IsNotFound(err) {
		t.Fatalf("AuthorizationPolicy created for a fleet outside the mesh: %v", err)
	}
	// Reservation hashes of fleets created before the field existed are unchanged.
	with := f.DeepCopy()
	with.Spec.Mesh = &fleet.MeshSpec{}
	if specHash(with) != specHash(f) {
		t.Fatal("mesh settings changed the storage reservation hash")
	}
}

func TestIstioMeshRendersSidecarEgressAndPolicy(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			f := meshFixture(profile)
			r := setup(t, f)
			reason(t, reconcile(t, r, f), "Provisioning")
			reason(t, reconcile(t, r, f), "Provisioning")

			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if !matches(workload(f, r.Options), w) {
				t.Fatal("workload does not carry the mesh template")
			}
			template := podTemplate(f, r.Options)
			if template.Labels[istioInjectLabel] != "true" || template.Labels[FleetNameLabel] != f.Name || template.Labels[FleetLabel] != string(f.UID) {
				t.Fatalf("pod labels: %v", template.Labels)
			}
			if template.Annotations[istioNativeSidecar] != "true" || template.Annotations[istioExcludeOutbound] != podIdentityAgentCIDR {
				t.Fatalf("pod annotations: %v", template.Annotations)
			}
			// The workload selector stays the fleet label, so it never changes.
			if sel := selector(f).MatchLabels; len(sel) != 1 || sel[FleetLabel] != string(f.UID) {
				t.Fatalf("selector changed: %v", sel)
			}

			p := &networkingv1.NetworkPolicy{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
				t.Fatal(err)
			}
			egress := istiodEgress(p)
			if egress == nil || egress.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "istio-system" || egress.To[0].PodSelector.MatchLabels["app"] != "istiod" || *egress.Ports[0].Protocol != corev1.ProtocolTCP {
				t.Fatalf("istiod egress: %+v", egress)
			}

			ap, err := authorizationPolicy(t, r, f)
			if err != nil {
				t.Fatal(err)
			}
			if !routingOwned(f, ap) {
				t.Fatal("AuthorizationPolicy is not owned by the fleet")
			}
			if owner := metav1.GetControllerOf(ap); owner == nil || owner.BlockOwnerDeletion == nil || *owner.BlockOwnerDeletion {
				t.Fatal("AuthorizationPolicy may block fleet deletion")
			}
			action, _, _ := unstructured.NestedString(ap.Object, "spec", "action")
			match, _, _ := unstructured.NestedStringMap(ap.Object, "spec", "selector", "matchLabels")
			if action != "ALLOW" || len(match) != 1 || match[FleetLabel] != string(f.UID) {
				t.Fatalf("policy action %q selector %v", action, match)
			}
			rules := policyRules(t, ap)
			if len(rules) != 2 {
				t.Fatalf("want peer and application rules, got %v", rules)
			}
			principals := rules[0]["from"].([]any)[0].(map[string]any)["source"].(map[string]any)["principals"].([]any)
			if !slices.Equal(principals, []any{"*/ns/fleets/sa/runtime", "*/ns/celld-system/sa/celld-operator"}) || !slices.Equal(rulePorts(rules[0]), []any{"8081"}) {
				t.Fatalf("peer rule: %v", rules[0])
			}
			if _, ok := rules[1]["from"]; ok || !slices.Equal(rulePorts(rules[1]), []any{"8080"}) {
				t.Fatalf("AllowAll application rule: %v", rules[1])
			}
		})
	}
}

// Mesh settings are mutable and converge in place without touching Pods.
func TestIstioMeshSettingsConverge(t *testing.T) {
	f := meshFixture("Bucket")
	r := setup(t, f)
	f = reconcile(t, r, f)
	f.Default()
	before, err := authorizationPolicy(t, r, f)
	if err != nil {
		t.Fatal(err)
	}
	template := podTemplate(f, r.Options)
	f.Spec.Mesh.Istio = fleet.IstioMeshSpec{ControlPlaneNamespace: "istio-canary", ApplicationAccess: fleet.ApplicationAccessPolicies}
	if err := r.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	r.Options.OperatorServiceAccount = "ops"
	reconcile(t, r, f)
	after, err := authorizationPolicy(t, r, f)
	if err != nil {
		t.Fatal(err)
	}
	if after.GetUID() != before.GetUID() {
		t.Fatal("AuthorizationPolicy replaced instead of updated")
	}
	rules := policyRules(t, after)
	if len(rules) != 1 || !slices.Equal(rulePorts(rules[0]), []any{"8081"}) {
		t.Fatalf("Policies access must leave port 8080 to user policies: %v", rules)
	}
	principals := rules[0]["from"].([]any)[0].(map[string]any)["source"].(map[string]any)["principals"].([]any)
	if principals[1] != "*/ns/celld-system/sa/ops" {
		t.Fatalf("operator identity: %v", principals)
	}
	p := &networkingv1.NetworkPolicy{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
		t.Fatal(err)
	}
	if e := istiodEgress(p); e == nil || e.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "istio-canary" {
		t.Fatalf("istiod egress not moved: %+v", e)
	}
	if !equality.Semantic.DeepEqual(template, podTemplate(f, r.Options)) {
		t.Fatal("mesh settings changed the Pod template")
	}
}

// No member starts without the peer-port policy: a cluster without Istio's
// API, or a same-named policy the fleet does not own, blocks provisioning.
func TestIstioMeshBlocksWithoutItsPolicy(t *testing.T) {
	cases := map[string]func(t *testing.T, r *Reconciler, f *fleet.CelldFleet){
		"no Istio API": func(t *testing.T, r *Reconciler, f *fleet.CelldFleet) {
			noMatch := &meta.NoKindMatchError{GroupKind: authorizationPolicyGVK.GroupKind(), SearchedVersions: []string{"v1"}}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == authorizationPolicyGVK {
					return noMatch
				}
				return c.Get(ctx, key, obj, opts...)
			}})
		},
		"foreign policy": func(t *testing.T, r *Reconciler, f *fleet.CelldFleet) {
			foreign := &unstructured.Unstructured{}
			foreign.SetGroupVersionKind(authorizationPolicyGVK)
			foreign.SetNamespace(f.Namespace)
			foreign.SetName(f.Name + "-mesh")
			foreign.Object["spec"] = map[string]any{"action": "DENY"}
			if err := r.Create(t.Context(), foreign); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := meshFixture("Bucket")
			r := setup(t, f)
			arrange(t, r, f)
			for range 2 {
				reason(t, reconcile(t, r, f), "InfrastructureBlocked")
			}
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); !apierrors.IsNotFound(err) {
				t.Fatalf("workload created without its AuthorizationPolicy: %v", err)
			}
		})
	}
}

// A fleet-namespace Role from before this release lacks the policy rule; the
// fleet names the Role remedy instead of a generic infrastructure failure.
func TestIstioMeshWithoutRoleRuleNamesTheRole(t *testing.T) {
	f := meshFixture("Bucket")
	r := setup(t, f)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == authorizationPolicyGVK {
			return apierrors.NewForbidden(authorizationPolicyGVK.GroupVersion().WithResource("authorizationpolicies").GroupResource(), key.Name, nil)
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	reason(t, reconcile(t, r, f), "NamespaceAccessDenied")
}

func peerRuleOpen(t *testing.T, r *Reconciler, f *fleet.CelldFleet) bool {
	t.Helper()
	ap, err := authorizationPolicy(t, r, f)
	if err != nil {
		t.Fatal(err)
	}
	_, restricted := policyRules(t, ap)[0]["from"]
	return !restricted
}

func peerAuthentication(t *testing.T, r *Reconciler, f *fleet.CelldFleet) (*unstructured.Unstructured, error) {
	t.Helper()
	obj := meshObject(f, peerAuthenticationGVK)
	err := r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj)
	return obj, err
}

// rollMesh runs the operator and the simulated workload controller until every
// member matches the desired membership, then reconciles once more.
func rollMesh(x *operationFixture) {
	x.t.Helper()
	for range 40 {
		x.operatorStep()
		x.syncWorkload()
		mixed, err := x.r.meshMixed(x.t.Context(), x.f)
		if err != nil {
			x.t.Fatal(err)
		}
		if !mixed {
			x.operatorStep()
			return
		}
	}
	x.t.Fatal("members never matched the desired membership")
}

// An existing fleet joins and leaves the mesh one member at a time. While
// members are mixed, the peer port accepts plaintext from any source the
// NetworkPolicy admits; once every member matches, the strict policy returns.
func TestMeshMembershipRollsOneMemberAtATime(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			x := newOperationFixture(t, profile)
			x.settle()
			if _, err := authorizationPolicy(t, x.r, x.f); !apierrors.IsNotFound(err) {
				t.Fatalf("policy before joining: %v", err)
			}

			x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = &fleet.MeshSpec{} })
			x.operatorStep()
			if !meshMember(x.workload()) {
				t.Fatal("template not moved into the mesh")
			}
			// The relaxed objects exist before the first member rolls.
			pa, err := peerAuthentication(t, x.r, x.f)
			if err != nil || !routingOwned(x.f, pa) {
				t.Fatalf("no transition PeerAuthentication: %v", err)
			}
			mode, _, _ := unstructured.NestedString(pa.Object, "spec", "portLevelMtls", "8081", "mode")
			if mode != "PERMISSIVE" {
				t.Fatalf("peer port mode %q", mode)
			}
			if !peerRuleOpen(t, x.r, x.f) {
				t.Fatal("peer rule requires identities while members are mixed")
			}
			x.syncWorkload() // one member rolls
			x.operatorStep()
			if !peerRuleOpen(t, x.r, x.f) {
				t.Fatal("strict policy returned mid-roll")
			}
			rollMesh(x)
			if peerRuleOpen(t, x.r, x.f) {
				t.Fatal("strict peer rule not restored after the roll")
			}
			if _, err := peerAuthentication(t, x.r, x.f); !apierrors.IsNotFound(err) {
				t.Fatalf("transition PeerAuthentication left behind: %v", err)
			}

			x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = nil })
			x.operatorStep()
			if meshMember(x.workload()) || !peerRuleOpen(t, x.r, x.f) {
				t.Fatal("leaving did not relax the peer rule before rolling")
			}
			if _, err := peerAuthentication(t, x.r, x.f); err != nil {
				t.Fatalf("no transition PeerAuthentication while leaving: %v", err)
			}
			rollMesh(x)
			policy := &networkingv1.NetworkPolicy{}
			if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), policy); err != nil {
				t.Fatal(err)
			}
			if istiodEgress(policy) != nil {
				t.Fatal("istiod egress left open after the final proxy exited")
			}
			for _, gvk := range []schema.GroupVersionKind{authorizationPolicyGVK, peerAuthenticationGVK} {
				obj := meshObject(x.f, gvk)
				if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
					t.Fatalf("%s left after leaving the mesh: %v", gvk.Kind, err)
				}
			}
		})
	}
}

// Namespace injection must not put replacement Pods back in the mesh after
// an explicit departure. The opt-out must survive later reconciles/restarts.
func TestMeshDepartureDisablesNamespaceInjection(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.settle()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = &fleet.MeshSpec{} })
	x.operatorStep()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = nil })
	x.operatorStep()
	if got := podTemplateOf(x.workload()).Labels[istioInjectLabel]; got != "false" {
		t.Fatalf("departing template must override namespace injection, got %q", got)
	}
	x.operatorStep()
	if got := podTemplateOf(x.workload()).Labels[istioInjectLabel]; got != "false" {
		t.Fatalf("opt-out lost on later reconcile, got %q", got)
	}
}

func TestMeshDepartureRetainsIstiodUntilLastProxyExits(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.settle()
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Mesh = &fleet.MeshSpec{Istio: fleet.IstioMeshSpec{ControlPlaneNamespace: "istio-canary"}}
	})
	x.operatorStep()
	// Model a native sidecar that is still draining after the celld container.
	p := x.pod("alpha-0")
	p.Spec.InitContainers = []corev1.Container{{Name: "istio-proxy", RestartPolicy: new(corev1.ContainerRestartPolicyAlways)}}
	p.Finalizers = []string{"test.example/hold"}
	if err := x.r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = nil })
	x.operatorStep()
	policy := &networkingv1.NetworkPolicy{}
	if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(x.f), policy); err != nil {
		t.Fatal(err)
	}
	if e := istiodEgress(policy); e == nil || e.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "istio-canary" {
		t.Fatalf("draining sidecar lost its control-plane egress: %+v", e)
	}
	if _, err := peerAuthentication(t, x.r, x.f); err != nil {
		t.Fatalf("draining sidecar lost transition policy: %v", err)
	}
}

func TestMeshMembershipRequiresActualSidecar(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.settle()
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = &fleet.MeshSpec{} })
	x.operatorStep()
	// A label is an injection request, not proof that admission injected a proxy.
	pods := &corev1.PodList{}
	if err := x.r.List(t.Context(), pods, client.InNamespace(x.f.Namespace), client.MatchingLabels(labels(x.f))); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		p.Labels[istioInjectLabel] = "true"
		if err := x.r.Update(t.Context(), &p); err != nil {
			t.Fatal(err)
		}
	}
	mixed, err := x.r.meshMixed(t.Context(), x.f)
	if err != nil {
		t.Fatal(err)
	}
	if !mixed {
		t.Fatal("request labels were mistaken for injected sidecars")
	}
}

func TestMeshMissingInjectionDoesNotReportProvisioned(t *testing.T) {
	x := newOperationFixture(t, "Bucket")
	x.settle()
	x.admit = func(spec *corev1.PodSpec) { spec.InitContainers = nil }
	x.edit(func(f *fleet.CelldFleet) { f.Spec.Mesh = &fleet.MeshSpec{} })
	for range 8 {
		x.operatorStep()
		x.syncWorkload()
	}
	if !rolledOut(x.workload()) {
		t.Fatal("workload fixture did not finish its roll")
	}
	reason(t, x.step(), "Provisioning")
	if !peerRuleOpen(t, x.r, x.f) {
		t.Fatal("missing injection restored the strict peer rule")
	}
}

func TestMeshManagedCleanupReportsMissingRole(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Annotations = map[string]string{istioHistoryAnnotation: "istio-system"}
	r := setup(t, f)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind() == authorizationPolicyGVK {
			return apierrors.NewForbidden(authorizationPolicyGVK.GroupVersion().WithResource("authorizationpolicies").GroupResource(), key.Name, nil)
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	reason(t, reconcile(t, r, f), "NamespaceAccessDenied")
}

func TestMeshDepartureWaitsForOrphanedProxy(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Annotations = map[string]string{istioHistoryAnnotation: "istio-system"}
	r := setup(t, f)
	p := &corev1.Pod{Name: "draining", Namespace: f.Namespace, Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "istio-proxy"}}}}
	if err := r.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	mixed, err := r.meshMixed(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !mixed {
		t.Fatal("workload absence hid an orphaned proxy")
	}
}
