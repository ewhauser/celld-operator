package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reliabilityClient traces every API boundary of a reconcile. A fault can
// reject a request, or discard a successful write's response after the backing
// client committed it. The latter models the ambiguous result of a connection
// loss; treating every timeout as an uncommitted write misses those failures.
type reliabilityClient struct {
	client.Client
	calls []reliabilityCall
	at    int // One-based call index; zero records a fault-free trace.
	after bool
	hit   bool
	err   error
}

type reliabilityCall struct {
	verb, object string
	write, ok    bool
}

type reliabilityFaultMode struct {
	name  string
	after bool
	err   error
}

func reliabilityFaultModes(call reliabilityCall, name string) []reliabilityFaultMode {
	modes := []reliabilityFaultMode{
		{"timeout", false, apierrors.NewTimeoutError("injected transport failure", 1)},
		{"throttled", false, apierrors.NewTooManyRequests("injected API throttling", 1)},
	}
	if call.write && call.ok {
		modes = append(modes, reliabilityFaultMode{"lost-response", true, apierrors.NewTimeoutError("write committed; response lost", 1)})
	}
	if call.verb == "update" || call.verb == "patch" || call.verb == "status-patch" || call.verb == "delete" {
		modes = append(modes, reliabilityFaultMode{"conflict", false, apierrors.NewConflict(schema.GroupResource{Resource: "injected"}, name, fmt.Errorf("concurrent resource version"))})
	}
	return modes
}

func (c *reliabilityClient) call(verb string, obj any, name string, write bool, invoke func() error) error {
	entry := reliabilityCall{verb: verb, object: fmt.Sprintf("%T/%s", obj, name), write: write}
	c.calls = append(c.calls, entry)
	i := len(c.calls) - 1
	injected := c.at == i+1
	if injected && !c.after {
		c.hit = true
		return c.err
	}
	err := invoke()
	c.calls[i].ok = err == nil
	if injected && c.after && err == nil {
		c.hit = true
		return c.err
	}
	return err
}

func (c *reliabilityClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.call("get", obj, key.String(), false, func() error { return c.Client.Get(ctx, key, obj, opts...) })
}
func (c *reliabilityClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.call("list", list, "", false, func() error { return c.Client.List(ctx, list, opts...) })
}
func (c *reliabilityClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.call("create", obj, client.ObjectKeyFromObject(obj).String(), true, func() error { return c.Client.Create(ctx, obj, opts...) })
}
func (c *reliabilityClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.call("update", obj, client.ObjectKeyFromObject(obj).String(), true, func() error { return c.Client.Update(ctx, obj, opts...) })
}
func (c *reliabilityClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.call("patch", obj, client.ObjectKeyFromObject(obj).String(), true, func() error { return c.Client.Patch(ctx, obj, patch, opts...) })
}
func (c *reliabilityClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.call("delete", obj, client.ObjectKeyFromObject(obj).String(), true, func() error { return c.Client.Delete(ctx, obj, opts...) })
}
func (c *reliabilityClient) Status() client.SubResourceWriter {
	return &reliabilityStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type reliabilityStatusWriter struct {
	client.SubResourceWriter
	c *reliabilityClient
}

func (w *reliabilityStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.c.call("status-patch", obj, client.ObjectKeyFromObject(obj).String(), true, func() error {
		return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	})
}

func reliabilitySetup(t *testing.T, profile, phase string) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "reliability-alpha", "Bucket")
	switch profile {
	case "Ordered":
		f.Spec.BucketWorkload = "Ordered"
	case "PersistentFleet":
		f = fixture("alpha", "reliability-alpha", profile)
	}
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	r := setup(t, f)
	r.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	if phase == "provision" {
		return r, f
	}
	reconcile(t, r, f)
	reconcile(t, r, f)
	if phase == "capacity-state" {
		return r, enableCapacity(t, r, f, "Shadow")
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatal(err)
	}
	switch w := w.(type) {
	case *appsv1.Deployment:
		w.Spec.Template.Spec.Containers[0].Image = "unexpected:latest"
	case *appsv1.StatefulSet:
		w.Spec.Template.Spec.Containers[0].Image = "unexpected:latest"
	}
	if err := r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	p := &networkingv1.NetworkPolicy{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Ingress = append(p.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{})
	if err := r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), pdb); err != nil {
		t.Fatal(err)
	}
	pdb.Spec.MaxUnavailable.IntVal = 2
	if err := r.Update(t.Context(), pdb); err != nil {
		t.Fatal(err)
	}
	return r, f
}

func reliabilityManifestsMatch(t *testing.T, r *Reconciler, f *fleet.CelldFleet) bool {
	t.Helper()
	for _, desired := range append(prerequisites(f, r.Options), workload(f, r.Options)) {
		actual := emptyObject(desired)
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(desired), actual); apierrors.IsNotFound(err) {
			return false
		} else if err != nil {
			t.Fatal(err)
		}
		if !matches(desired, actual) {
			return false
		}
	}
	if f.Spec.Capacity != nil {
		res := envReservation(t, r, f)
		state, err := readState(res)
		if err != nil {
			t.Fatal(err)
		}
		if state == nil || state.Capacity == nil || state.Capacity.Decision.Mode != "Shadow" {
			return false
		}
	}
	return true
}

func reliabilitySafety(t *testing.T, r *Reconciler, f *fleet.CelldFleet, initial bool) {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	reserved := err == nil
	if reserved && (res.Spec.FleetUID != string(f.UID) || res.Spec.FleetNamespace != f.Namespace || res.Spec.Bucket != f.Spec.Storage.Bucket || len(res.OwnerReferences) != 0 || !res.DeletionTimestamp.IsZero()) {
		t.Fatalf("storage authority changed after API fault: %+v", res)
	}
	w := emptyObject(workload(f, r.Options))
	err = r.Get(t.Context(), client.ObjectKeyFromObject(f), w)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err == nil {
		if !reserved || w.GetLabels()[FleetLabel] != string(f.UID) || len(w.GetOwnerReferences()) != 0 || replicas(w) != f.Spec.Replicas {
			t.Fatalf("workload lost ownership, reservation, or replica intent after API fault: %+v", w)
		}
		if initial {
			for _, desired := range prerequisites(f, r.Options) {
				actual := emptyObject(desired)
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(desired), actual); err != nil || !matches(desired, actual) {
					t.Fatalf("created compute without verified prerequisite %T %s: %v", desired, desired.GetName(), err)
				}
			}
		}
	}
	got := &fleet.CelldFleet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ReadyReplicas != 0 || meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Fatal("fault/replay claimed readiness without a workload-controller observation")
	}
}

// Sweep every boundary in a successful reconcile rather than guessing which
// handful of failures are worth testing. Every case starts from fresh state,
// loses process memory after the fault, and has a bounded convergence check.
// Trace indices appear in subtest names so failures are directly reproducible.
func TestReliabilityAPIFaultSweep(t *testing.T) {
	for _, profile := range []string{"Deployment", "Ordered", "PersistentFleet"} {
		for _, phase := range []string{"provision", "drift", "capacity-state"} {
			t.Run(profile+"/"+phase, func(t *testing.T) {
				r, f := reliabilitySetup(t, profile, phase)
				trace := &reliabilityClient{Client: r.Client}
				r.Client = trace
				if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
					t.Fatal(err)
				}
				for i, call := range trace.calls {
					for _, mode := range reliabilityFaultModes(call, f.Name) {
						t.Run(fmt.Sprintf("%02d-%s-%s/%s", i+1, call.verb, call.object, mode.name), func(t *testing.T) {
							r, f := reliabilitySetup(t, profile, phase)
							base := r.Client
							faults := &reliabilityClient{Client: base, at: i + 1, after: mode.after, err: mode.err}
							r.Client = faults
							// Some best-effort observations absorb the error and mark
							// their projection invalid; an error return is not required.
							_, _ = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
							if !faults.hit {
								t.Fatalf("API boundary was not reached: %+v", faults.calls)
							}
							for _, call := range faults.calls {
								if call.verb == "delete" {
									t.Fatal("API failure triggered resource deletion")
								}
							}
							// Construct a new reconciler, keeping only Kubernetes state.
							r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true, now: r.now}
							reliabilitySafety(t, r, f, phase == "provision")
							converged := false
							for range 8 {
								if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
									t.Fatal(err)
								}
								reliabilitySafety(t, r, f, phase == "provision")
								if reliabilityManifestsMatch(t, r, f) {
									converged = true
									break
								}
							}
							if !converged {
								t.Fatal("controller failed to converge within eight retries after API recovery")
							}
						})
					}
				}
			})
		}
	}
}

func reliabilityDeletionSetup(t *testing.T, profile string) (*Reconciler, *fleet.CelldFleet) {
	t.Helper()
	r, f := reliabilitySetup(t, profile, "provision")
	reconcile(t, r, f)
	reconcile(t, r, f)
	if err := r.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	return r, f
}

func reliabilityDeletionSafety(t *testing.T, r *Reconciler, f *fleet.CelldFleet) bool {
	t.Helper()
	res := envReservation(t, r, f)
	if res.Spec.FleetUID != string(f.UID) || !res.DeletionTimestamp.IsZero() || len(res.OwnerReferences) != 0 {
		t.Fatal("deletion replay changed the permanent storage reservation")
	}
	for _, desired := range prerequisites(f, r.Options) {
		actual := emptyObject(desired)
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(desired), actual); err != nil || !matches(desired, actual) {
			t.Fatalf("deletion replay removed or mutated retained prerequisite %T %s: %v", desired, desired.GetName(), err)
		}
	}
	got := &fleet.CelldFleet{}
	err := r.Get(t.Context(), client.ObjectKeyFromObject(f), got)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err == nil {
		if got.DeletionTimestamp.IsZero() {
			t.Fatal("deletion replay cleared the deletion request")
		}
		return false
	}
	w := emptyObject(workload(f, r.Options))
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); !apierrors.IsNotFound(err) {
		t.Fatalf("fleet finalizer released while compute remained: %v", err)
	}
	return true
}

// Deletion spans two reconciles: compute removal, then finalizer release after
// observing absence. Fault both halves, including writes that commit before
// their response disappears. Restart/replay must finish cleanup without ever
// recreating compute or removing the permanent scope and retained services.
func TestReliabilityDeletionAPIFaultSweep(t *testing.T) {
	for _, profile := range []string{"Deployment", "Ordered", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, f := reliabilityDeletionSetup(t, profile)
			trace := &reliabilityClient{Client: r.Client}
			r.Client = trace
			for range 2 {
				if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
					t.Fatal(err)
				}
			}
			for i, call := range trace.calls {
				for _, mode := range reliabilityFaultModes(call, f.Name) {
					t.Run(fmt.Sprintf("%02d-%s-%s/%s", i+1, call.verb, call.object, mode.name), func(t *testing.T) {
						r, f := reliabilityDeletionSetup(t, profile)
						base := r.Client
						faults := &reliabilityClient{Client: base, at: i + 1, after: mode.after, err: mode.err}
						r.Client = faults
						for range 2 {
							_, _ = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
						}
						if !faults.hit {
							t.Fatalf("deletion API boundary was not reached: %+v", faults.calls)
						}
						for _, call := range faults.calls {
							if call.verb == "create" {
								t.Fatal("deletion replay attempted resource creation")
							}
						}
						// Retain recording across retries while dropping fault injection.
						faults.at = 0
						r = &Reconciler{Client: faults, Options: r.Options, NetworkPolicyEnforced: true, now: r.now}
						inspector := &Reconciler{Client: base, Options: r.Options}
						complete := reliabilityDeletionSafety(t, inspector, f)
						for n := 0; !complete && n < 8; n++ {
							if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
								t.Fatal(err)
							}
							complete = reliabilityDeletionSafety(t, inspector, f)
						}
						if !complete {
							t.Fatal("deletion failed to finish within eight retries after API recovery")
						}
						for _, call := range faults.calls {
							if call.verb == "create" {
								t.Fatal("deletion replay attempted resource creation")
							}
						}
					})
				}
			}
		})
	}
}

// A malformed paginated membership response must be treated as incomplete,
// even when every Pod returned is valid. Contraction cannot infer the missing
// members' survivor capacity from a first page.
func TestReliabilityPartialMembershipBlocksContraction(t *testing.T) {
	x := newOperationFixture(t, "Deployment")
	x.f = enableCapacity(t, x.r, x.f, "External")
	x.desired(2)
	base := x.r.Client
	x.r.Client = &reliabilityPartialPodsClient{Client: base}
	got := x.step()
	if replicas(x.workload()) != 3 {
		t.Fatal("contracted using a truncated Pod list")
	}
	reason(t, got, "CapacityUncertain")
	if got.Status.ReplicaObservationValid {
		t.Fatal("truncated Pod observation reported as complete")
	}
	x.r.Client = base
	x.converge()
	if replicas(x.workload()) != 2 {
		t.Fatal("contraction did not resume after complete membership reads returned")
	}
}

type reliabilityPartialPodsClient struct{ client.Client }

func (c *reliabilityPartialPodsClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*corev1.PodList); ok {
		pods.Continue = "injected-next-page"
	}
	return nil
}
