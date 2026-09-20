package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The envtest suite runs the reconciler against a real kube-apiserver and etcd
// (no kubelet, scheduler or workload controllers). It exists to exercise what the
// fake client cannot: CRD CEL admission and defaulting, real resourceVersion
// conflicts on the journal and workload CAS, and merge patches with optimistic
// locking. Tests skip unless KUBEBUILDER_ASSETS points at envtest binaries; use
// `make test-envtest`.
var (
	envtestOnce   sync.Once
	envtestConfig *rest.Config
	envtestEnv    *envtest.Environment
	envtestErr    error
	envtestSeq    atomic.Int32
)

func TestMain(m *testing.M) {
	code := m.Run()
	if envtestEnv != nil {
		_ = envtestEnv.Stop()
	}
	os.Exit(code)
}

func envtestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := fleet.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// envtestClient returns an uncached client with watch support against the shared
// API server, matching how cmd/celld-operator constructs the reconciler's client.
func envtestClient(t *testing.T) client.WithWatch {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make test-envtest`")
	}
	envtestOnce.Do(func() {
		envtestEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
			ErrorIfCRDPathMissing: true,
		}
		envtestConfig, envtestErr = envtestEnv.Start()
	})
	if envtestErr != nil {
		t.Fatalf("envtest start: %v", envtestErr)
	}
	c, err := client.NewWithWatch(envtestConfig, client.Options{Scheme: envtestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type envtestFixture struct {
	client    client.WithWatch
	namespace string
	fleet     *fleet.CelldFleet
}

// envtestSetup creates an isolated namespace, the referenced ServiceAccount, the
// shared retained StorageClass, and a fleet with a unique bucket, then returns a
// reconciler wired exactly as production is (uncached client, no fake seams).
func envtestSetup(t *testing.T, profile string) (*Reconciler, *envtestFixture) {
	t.Helper()
	c := envtestClient(t)
	ctx := t.Context()
	n := envtestSeq.Add(1)
	ns := fmt.Sprintf("fleets-%d-%d", n, time.Now().UnixNano()%1000000)
	if err := c.Create(ctx, &corev1.Namespace{Name: ns}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.ServiceAccount{Name: "runtime", Namespace: ns}); err != nil {
		t.Fatal(err)
	}
	sc := &storagev1.StorageClass{
		Name:              "retained",
		Provisioner:       "ebs.csi.aws.com",
		ReclaimPolicy:     ptr.To(corev1.PersistentVolumeReclaimRetain),
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	if err := c.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	f := fixture("alpha", fmt.Sprintf("bucket-%s", ns), profile)
	f.Namespace = ns
	f.UID = "" // The API server assigns identity; the fake fixture's UID is not accepted.
	f.Spec.Placement.AZCount = 1
	f.Spec.Placement.Zones = []string{"us-east-1a"}
	if err := c.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, Options: Options{OperatorNamespace: "celld-system"}, NetworkPolicyEnforced: true}
	return r, &envtestFixture{client: c, namespace: ns, fleet: f}
}

func envtestFreshReconciler(r *Reconciler) *Reconciler {
	return &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: r.NetworkPolicyEnforced}
}

// provision reconciles until the workload exists, as the fake-client tests do.
func (x *envtestFixture) provision(t *testing.T, r *Reconciler) {
	t.Helper()
	reason(t, reconcile(t, r, x.fleet), "Provisioning")
	reason(t, reconcile(t, r, x.fleet), "Provisioning")
	w := workload(x.fleet, r.Options)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), w); err != nil {
		t.Fatalf("workload not created through the real API server: %v", err)
	}
}

func envtestReservation(t *testing.T, c client.Client, f *fleet.CelldFleet) *fleet.CelldStorageReservation {
	t.Helper()
	res := &fleet.CelldStorageReservation{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEnvtestAdmissionDefaultsAndImmutability(t *testing.T) {
	c := envtestClient(t)
	ctx := t.Context()
	ns := fmt.Sprintf("admission-%d", envtestSeq.Add(1))
	if err := c.Create(ctx, &corev1.Namespace{Name: ns}); err != nil {
		t.Fatal(err)
	}
	base := func() *fleet.CelldFleet {
		return &fleet.CelldFleet{
			Name: "alpha", Namespace: ns,
			Spec: fleet.CelldFleetSpec{
				Qualification:      "Experimental",
				Profile:            "Bucket",
				ServiceAccountName: "runtime",
				Storage:            fleet.StorageSpec{Bucket: "bucket-" + ns, Region: "us-east-1"},
				Placement:          fleet.PlacementSpec{AZCount: 1, Zones: []string{"us-east-1a"}},
			},
		}
	}

	// Server-side defaults come from the CRD schema, not the Go Default() method.
	f := base()
	if err := c.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	if f.Spec.Replicas != 3 || f.Spec.Storage.SizeGiB != 10 || f.Spec.Placement.Mode != "Strict" {
		t.Fatalf("CRD defaults not applied by the API server: %+v", f.Spec)
	}

	// CEL cross-field rules reject at creation time.
	invalid := []struct {
		name string
		edit func(*fleet.CelldFleet)
	}{
		{"azCount differs from zones", func(f *fleet.CelldFleet) { f.Spec.Placement.AZCount = 2 }},
		{"replicas below azCount", func(f *fleet.CelldFleet) {
			f.Spec.Placement = fleet.PlacementSpec{AZCount: 2, Zones: []string{"us-east-1a", "us-east-1b"}}
			f.Spec.Replicas = 1
		}},
		{"zone outside region", func(f *fleet.CelldFleet) { f.Spec.Placement.Zones = []string{"us-west-2a"} }},
		{"storageClassName on Bucket", func(f *fleet.CelldFleet) { f.Spec.Storage.StorageClassName = "retained" }},
		{"PersistentFleet without storageClassName", func(f *fleet.CelldFleet) { f.Spec.Profile = "PersistentFleet" }},
		{"production qualification", func(f *fleet.CelldFleet) { f.Spec.Qualification = "Production" }},
		{"capacity minimum below azCount", func(f *fleet.CelldFleet) {
			f.Spec.Placement = fleet.PlacementSpec{AZCount: 2, Zones: []string{"us-east-1a", "us-east-1b"}}
			f.Spec.Capacity = &fleet.CapacityPolicy{MinReplicas: 1}
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			f.Name = "invalid"
			tc.edit(f)
			err := c.Create(ctx, f)
			if !apierrors.IsInvalid(err) {
				t.Fatalf("admission accepted %s: %v", tc.name, err)
			}
		})
	}

	// Only replicas, capacity, runtimeImage and maintenance may change after creation.
	for _, tc := range []struct {
		name string
		edit func(*fleet.CelldFleet)
	}{
		{"profile", func(f *fleet.CelldFleet) {
			f.Spec.Profile = "PersistentFleet"
			f.Spec.Storage.StorageClassName = "retained"
		}},
		{"bucket", func(f *fleet.CelldFleet) { f.Spec.Storage.Bucket = "other-" + ns }},
		{"zones", func(f *fleet.CelldFleet) { f.Spec.Placement.Zones = []string{"us-east-1b"} }},
		{"serviceAccountName", func(f *fleet.CelldFleet) { f.Spec.ServiceAccountName = "other" }},
	} {
		t.Run("immutable "+tc.name, func(t *testing.T) {
			got := &fleet.CelldFleet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
				t.Fatal(err)
			}
			tc.edit(got)
			if err := c.Update(ctx, got); !apierrors.IsInvalid(err) {
				t.Fatalf("immutability bypassed for %s: %v", tc.name, err)
			}
		})
	}
	got := &fleet.CelldFleet{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = 4
	got.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true}
	if err := c.Update(ctx, got); err != nil {
		t.Fatalf("mutable fields rejected: %v", err)
	}

	// The cluster-scoped reservation is frozen entirely.
	res := &fleet.CelldStorageReservation{Name: "s3-" + ns, Spec: fleet.ReservationSpec{InitialReplicas: 3, Bucket: "b", FleetNamespace: ns, FleetName: "alpha", FleetUID: "u", SpecHash: "h"}}
	if err := c.Create(ctx, res); err != nil {
		t.Fatal(err)
	}
	res.Spec.FleetUID = "another"
	if err := c.Update(ctx, res); !apierrors.IsInvalid(err) {
		t.Fatalf("reservation transfer accepted: %v", err)
	}
}

func TestEnvtestFinalizerPatchPreservesConcurrentFinalizers(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	ctx := t.Context()
	f := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(x.fleet), f); err != nil {
		t.Fatal(err)
	}
	f.Finalizers = append(f.Finalizers, "example.com/other-controller")
	if err := r.Update(ctx, f); err != nil {
		t.Fatal(err)
	}
	got := reconcile(t, r, x.fleet)
	var ours, theirs bool
	for _, fin := range got.Finalizers {
		ours = ours || fin == Finalizer
		theirs = theirs || fin == "example.com/other-controller"
	}
	if !ours || !theirs {
		t.Fatalf("finalizer merge patch lost an entry: %v", got.Finalizers)
	}
}

func TestEnvtestProvisionAndJournaledScaleOut(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, x := envtestSetup(t, profile)
			x.provision(t, r)
			res := envtestReservation(t, r, x.fleet)
			if res.Spec.FleetUID != string(reconcile(t, r, x.fleet).UID) || res.Spec.InitialReplicas != 3 {
				t.Fatalf("reservation does not bind the server-assigned fleet UID: %+v", res.Spec)
			}
			f := desiredCount(t, r, x.fleet, 5)
			for range 8 {
				r = envtestFreshReconciler(r) // Every reconcile is a cold start: no in-memory state may carry the operation.
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Applied != 5 || j.Operation != nil {
				t.Fatalf("scale-out did not complete against the real API server: %+v", j)
			}
			w := workload(f, r.Options)
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if replicas(w) != 5 {
				t.Fatalf("workload replicas %d", replicas(w))
			}
			if profile == "PersistentFleet" {
				if len(j.Claims) != 5 {
					t.Fatalf("expected five exclusively created claim UIDs, got %d", len(j.Claims))
				}
				for name, uid := range j.Claims {
					pvc := &corev1.PersistentVolumeClaim{}
					if err := r.Get(t.Context(), types.NamespacedName{Namespace: x.namespace, Name: name}, pvc); err != nil {
						t.Fatal(err)
					}
					if pvc.UID != uid {
						t.Fatalf("journal records %s for %s but the server assigned %s", uid, name, pvc.UID)
					}
				}
			}
			// Status is a projection: wiping it through the real status subresource
			// must not alter the journal's applied count.
			f = &fleet.CelldFleet{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), f); err != nil {
				t.Fatal(err)
			}
			f.Status = fleet.CelldFleetStatus{}
			if err := r.Status().Update(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, f)
			if getJournal(t, r, f).Applied != 5 {
				t.Fatal("status loss erased applied capacity")
			}
		})
	}
}

func TestEnvtestJournalWriteLosesOnStaleResourceVersion(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	ctx := t.Context()
	first := envtestReservation(t, r, x.fleet)
	second := envtestReservation(t, r, x.fleet)
	j1, err := readJournal(first)
	if err != nil || j1 == nil {
		t.Fatalf("journal missing after provisioning: %v", err)
	}
	j2, err := readJournal(second)
	if err != nil {
		t.Fatal(err)
	}
	// The first writer must change content: the API server does not advance
	// resourceVersion for a byte-identical update, so a no-op write would leave the
	// second reader current rather than stale.
	j1.Inventory.CheckedAt = time.Now().UTC().Truncate(time.Second)
	if err := r.saveJournal(ctx, first, j1); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion == second.ResourceVersion {
		t.Fatal("first write did not advance the reservation resourceVersion")
	}
	err = r.saveJournal(ctx, second, j2)
	if !apierrors.IsConflict(err) {
		t.Fatalf("second writer with the stale resourceVersion must lose with 409 Conflict, got %v", err)
	}
	// saveJournal never refreshes the version; a retry on the same object still fails.
	if err := r.saveJournal(ctx, second, j2); !apierrors.IsConflict(err) {
		t.Fatalf("stale journal retry was accepted: %v", err)
	}
}

func TestEnvtestDelayedIssuerLosesWorkloadCASAfterPause(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, x := envtestSetup(t, profile)
			x.provision(t, r)
			ctx := t.Context()
			f := desiredCount(t, r, x.fleet, 5)
			reconcile(t, r, f)
			j := getJournal(t, r, f)
			if j.Operation == nil || j.Operation.Phase != "Intent" {
				t.Fatalf("expected durable intent before any effect: %+v", j.Operation)
			}
			op := *j.Operation
			stale := emptyObject(workload(f, r.Options))
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), stale); err != nil {
				t.Fatal(err)
			}
			if stale.GetResourceVersion() != op.WorkloadVersion {
				t.Fatalf("intent recorded workload version %s but server has %s", op.WorkloadVersion, stale.GetResourceVersion())
			}
			f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
			reason(t, reconcile(t, r, f), "MaintenancePaused")
			fenced := emptyObject(workload(f, r.Options))
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), fenced); err != nil {
				t.Fatal(err)
			}
			if fenced.GetAnnotations()[maintenanceFenceKey] == "" || fenced.GetResourceVersion() == op.WorkloadVersion {
				t.Fatal("pause did not CAS a fence onto the workload")
			}
			// The delayed issuer holds the pre-pause object: no fence is visible to it and
			// the recorded version matches, so the only thing stopping it is the API
			// server's resourceVersion precondition.
			err := r.applyReplicas(ctx, stale, &op)
			if !apierrors.IsConflict(err) {
				t.Fatalf("delayed issuer must lose the CAS with 409 Conflict, got %v", err)
			}
			after := emptyObject(workload(f, r.Options))
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), after); err != nil {
				t.Fatal(err)
			}
			if replicas(after) != 3 || after.GetAnnotations()[operationKey] != "" {
				t.Fatalf("fenced workload was modified: replicas=%d annotations=%v", replicas(after), after.GetAnnotations())
			}
			// A fresh read sees the fence and refuses before contacting the server.
			if err := r.applyReplicas(ctx, after, &op); err == nil || apierrors.IsConflict(err) {
				t.Fatalf("fresh issuer must refuse on the fence itself: %v", err)
			}
		})
	}
}

func TestEnvtestCrashAfterReplicaCASReconstructsOnce(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) {
			r, x := envtestSetup(t, profile)
			x.provision(t, r)
			ctx := t.Context()
			f := desiredCount(t, r, x.fleet, 4)
			reconcile(t, r, f)
			// PersistentFleet allocates claims before the replica effect; drive to the
			// phase immediately preceding the workload CAS.
			for range 3 {
				if j := getJournal(t, r, f); j.Operation != nil && j.Operation.Phase == "Prepared" {
					break
				}
				reconcile(t, r, f)
			}
			before := getJournal(t, r, f)
			if before.Operation == nil {
				t.Fatalf("operation completed before the crash could be injected: %+v", before)
			}
			opID := before.Operation.ID
			base := r.Client
			crashed := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*fleet.CelldStorageReservation); ok && !crashed {
					w := emptyObject(workload(f, r.Options))
					if err := c.Get(ctx, client.ObjectKeyFromObject(f), w); err == nil && replicas(w) == 4 {
						crashed = true
						return errors.New("leader crashed after replica CAS, before journal transition")
					}
				}
				return c.Update(ctx, obj, opts...)
			}})
			for range 4 {
				if crashed {
					break
				}
				_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			}
			if !crashed {
				t.Fatal("replica effect was never issued; crash point not reached")
			}
			issued := emptyObject(workload(f, r.Options))
			if err := base.Get(ctx, client.ObjectKeyFromObject(f), issued); err != nil {
				t.Fatal(err)
			}
			if replicas(issued) != 4 || issued.GetAnnotations()[operationKey] != opID {
				t.Fatalf("effect not durable on the server: replicas=%d op=%s", replicas(issued), issued.GetAnnotations()[operationKey])
			}
			if getJournal(t, &Reconciler{Client: base}, f).Applied != 3 {
				t.Fatal("journal advanced despite the injected crash")
			}
			// A new leader with no memory reconstructs the issued effect from the
			// workload's operation ID and exact count, without a second effect.
			r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true}
			for range 6 {
				r = envtestFreshReconciler(r)
				reconcile(t, r, f)
			}
			j := getJournal(t, r, f)
			if j.Applied != 4 || j.Operation != nil {
				t.Fatalf("reconstruction failed: %+v", j)
			}
			completed := 0
			for _, h := range j.History {
				if h.ID == opID {
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("operation %s recorded %d times in history", opID, completed)
			}
			final := emptyObject(workload(f, r.Options))
			if err := r.Get(ctx, client.ObjectKeyFromObject(f), final); err != nil {
				t.Fatal(err)
			}
			if replicas(final) != 4 {
				t.Fatalf("duplicate or reverted effect: replicas=%d", replicas(final))
			}
		})
	}
}

func TestEnvtestCorruptJournalBlocksWithoutRewrite(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	ctx := t.Context()
	res := envtestReservation(t, r, x.fleet)
	res.Annotations[journalKey] = `{"Version":99,"Initial":3,"Applied":3}`
	if err := r.Update(ctx, res); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r = envtestFreshReconciler(r)
		reason(t, reconcile(t, r, x.fleet), "JournalInvalid")
	}
	again := envtestReservation(t, r, x.fleet)
	if again.Annotations[journalKey] != res.Annotations[journalKey] || again.ResourceVersion != res.ResourceVersion {
		t.Fatal("unreadable journal was rewritten; evidence must be retained for review")
	}
	w := workload(x.fleet, r.Options)
	if err := r.Get(ctx, client.ObjectKeyFromObject(x.fleet), w); err != nil {
		t.Fatal(err)
	}
	if replicas(w) != 3 {
		t.Fatal("workload changed while the journal was unreadable")
	}
}

func TestEnvtestTuningAdmission(t *testing.T) {
	c := envtestClient(t)
	ctx := t.Context()
	ns := fmt.Sprintf("tuning-%d", envtestSeq.Add(1))
	if err := c.Create(ctx, &corev1.Namespace{Name: ns}); err != nil {
		t.Fatal(err)
	}
	base := func(name string) *fleet.CelldFleet {
		return &fleet.CelldFleet{Name: name, Namespace: ns, Spec: fleet.CelldFleetSpec{
			Qualification: "Experimental", Profile: "Bucket", ServiceAccountName: "runtime",
			Storage:   fleet.StorageSpec{Bucket: "bucket-" + ns + "-" + name, Region: "us-east-1"},
			Placement: fleet.PlacementSpec{AZCount: 1, Zones: []string{"us-east-1a"}},
		}}
	}
	// CEL cross-field rules on the tuning blocks reject at admission.
	for name, edit := range map[string]func(*fleet.CelldFleet){
		"cpu limit below request": func(f *fleet.CelldFleet) { f.Spec.Execution = &fleet.ExecutionSpec{CPURequest: "2", CPULimit: "1"} },
		"memory limit below request": func(f *fleet.CelldFleet) {
			f.Spec.Execution = &fleet.ExecutionSpec{MemoryRequest: "2Gi", MemoryLimit: "1Gi"}
		},
		"grace inside shutdown": func(f *fleet.CelldFleet) {
			f.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 60, TerminationGraceSeconds: 60}
		},
		"long shutdown default grace": func(f *fleet.CelldFleet) {
			f.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 40}
		},
		"malformed quantity": func(f *fleet.CelldFleet) { f.Spec.Execution = &fleet.ExecutionSpec{MemoryRequest: "lots"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := base("invalid")
			edit(f)
			if err := c.Create(ctx, f); !apierrors.IsInvalid(err) {
				t.Fatalf("admission accepted %s: %v", name, err)
			}
		})
	}
	// Valid tuning is accepted; equal quantities in different spellings compare equal.
	f := base("tuned")
	f.Spec.Execution = &fleet.ExecutionSpec{CPURequest: "1000m", CPULimit: "1", MemoryRequest: "1Gi", MemoryLimit: "1024Mi", MaxResidentCells: 400, IdleEvictSeconds: 60}
	f.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 120, TerminationGraceSeconds: 180}
	if err := c.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	// Both blocks are immutable, including adding or removing them.
	for name, edit := range map[string]func(*fleet.CelldFleet){
		"change cpu":       func(f *fleet.CelldFleet) { f.Spec.Execution.CPURequest = "2" },
		"change shutdown":  func(f *fleet.CelldFleet) { f.Spec.Lifecycle.ShutdownSeconds = 100 },
		"remove execution": func(f *fleet.CelldFleet) { f.Spec.Execution = nil },
	} {
		t.Run("immutable "+name, func(t *testing.T) {
			got := &fleet.CelldFleet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
				t.Fatal(err)
			}
			edit(got)
			if err := c.Update(ctx, got); !apierrors.IsInvalid(err) {
				t.Fatalf("tuning mutation accepted (%s): %v", name, err)
			}
		})
	}
	plain := base("plain")
	if err := c.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	plain.Spec.Lifecycle = &fleet.LifecycleSpec{ShutdownSeconds: 10, TerminationGraceSeconds: 30}
	if err := c.Update(ctx, plain); !apierrors.IsInvalid(err) {
		t.Fatalf("adding tuning after creation accepted: %v", err)
	}
	// Mutable fields still change on a tuned fleet.
	got := &fleet.CelldFleet{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = 4
	if err := c.Update(ctx, got); err != nil {
		t.Fatalf("replicas rejected on a tuned fleet: %v", err)
	}
}

// The /scale subresource is served by the API server from the CRD definition.
// An HPA reads status.replicas and the selector and writes spec.replicas.
func TestEnvtestScaleSubresource(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	x.provision(t, r)
	ctx := t.Context()
	reconcile(t, r, x.fleet)
	scale := &autoscalingv1.Scale{}
	if err := r.SubResource("scale").Get(ctx, x.fleet, scale); err != nil {
		t.Fatalf("scale subresource unavailable: %v", err)
	}
	if scale.Spec.Replicas != 3 || scale.Status.Selector != FleetLabel+"="+string(x.fleet.UID) {
		t.Fatalf("scale view %+v", scale)
	}
	// No kubelet runs here, so no pods exist and status.replicas is zero; the
	// field itself must be present for the autoscaler contract.
	got := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(x.fleet), got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.ReplicaObservationValid || got.Status.LabelSelector == "" {
		t.Fatalf("scale status not projected: %+v", got.Status)
	}
	scale.Spec.Replicas = 4
	if err := r.SubResource("scale").Update(ctx, x.fleet, client.WithSubResourceBody(scale)); err != nil {
		t.Fatalf("scale update rejected: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(x.fleet), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas != 4 {
		t.Fatalf("scale write did not land in spec.replicas: %d", got.Spec.Replicas)
	}
	// A scale write below azCount is refused by the CRD's own validation.
	scale.Spec.Replicas = 0
	if err := r.SubResource("scale").Update(ctx, x.fleet, client.WithSubResourceBody(scale)); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid scale accepted: %v", err)
	}
}
