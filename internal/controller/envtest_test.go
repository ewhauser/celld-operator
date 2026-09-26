package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The envtest suite runs the reconciler against a real kube-apiserver and etcd
// (no kubelet, scheduler or workload controllers). It exists to exercise what the
// fake client cannot: CRD CEL admission and defaulting, real resourceVersion
// conflicts on the reservation and workload CAS, and merge patches with optimistic
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
// shared disposable StorageClass, and a fleet with a unique bucket, then returns a
// reconciler wired exactly as production is (uncached client, no fake seams).
func envtestSetup(t *testing.T, profile string) (*Reconciler, *envtestFixture) {
	t.Helper()
	return envtestSetupWith(t, profile, nil)
}

func envtestSetupWith(t *testing.T, profile string, edit func(*fleet.CelldFleet)) (*Reconciler, *envtestFixture) {
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
		Name:              "disposable",
		Provisioner:       "ebs.csi.aws.com",
		ReclaimPolicy:     ptr.To(corev1.PersistentVolumeReclaimDelete),
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
	if edit != nil {
		edit(f)
	}
	if err := c.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: c, Options: Options{OperatorNamespace: "celld-system"}, NetworkPolicyEnforced: true}
	return r, &envtestFixture{client: c, namespace: ns, fleet: f}
}

func (x *envtestFixture) provision(t *testing.T, r *Reconciler) {
	t.Helper()
	reason(t, reconcile(t, r, x.fleet), "Provisioning")
	reason(t, reconcile(t, r, x.fleet), "Provisioning")
	w := workload(x.fleet, r.Options)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), w); err != nil {
		t.Fatalf("workload not created through the real API server: %v", err)
	}
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
	withEnv := base()
	withEnv.Name = "env-valid"
	value := "debug"
	withEnv.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_LOG", Value: &value}, {Name: "CELLD_API_TOKEN", SecretKeyRef: &fleet.SecretKeyRef{Name: "runtime-auth", Key: "token"}}}
	if err := c.Create(ctx, withEnv); err != nil {
		t.Fatalf("valid literal and Secret env rejected: %v", err)
	}
	mirrored := base()
	mirrored.Name = "mirrored"
	mirrored.Spec.RuntimeImage = "123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld@sha256:" + strings.Repeat("a", 64)
	if err := c.Create(ctx, mirrored); err != nil {
		t.Fatalf("mirrored runtime pin rejected: %v", err)
	}
	withTelemetry := base()
	withTelemetry.Name = "telemetry-valid"
	withTelemetry.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "http://collector:4318", Egress: fleet.CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "0.25"}
	if err := c.Create(ctx, withTelemetry); err != nil {
		t.Fatalf("valid telemetry rejected: %v", err)
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
		{"storageClassName on Bucket", func(f *fleet.CelldFleet) { f.Spec.Storage.StorageClassName = "disposable" }},
		{"PersistentFleet without storageClassName", func(f *fleet.CelldFleet) { f.Spec.Profile = "PersistentFleet" }},
		{"runtime tag", func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = "example.com/celld:latest" }},
		{"owned env", func(f *fleet.CelldFleet) {
			v := "bad"
			f.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_BUCKET", Value: &v}}
		}},
		{"duplicate env", func(f *fleet.CelldFleet) {
			v := "debug"
			f.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_LOG", Value: &v}, {Name: "CELLD_LOG", Value: &v}}
		}},
		{"missing env value", func(f *fleet.CelldFleet) { f.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_LOG"}} }},
		{"telemetry missing egress", func(f *fleet.CelldFleet) {
			f.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "http://collector:4318"}
		}},
		{"telemetry invalid ratio", func(f *fleet.CelldFleet) {
			f.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "http://collector:4318", Egress: fleet.CollectorEgress{PodLabels: map[string]string{"app": "otel"}}, Sampler: "traceidratio", SamplerArg: "2.0"}
		}},
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
			f.Spec.Storage.StorageClassName = "disposable"
		}},
		{"bucket", func(f *fleet.CelldFleet) { f.Spec.Storage.Bucket = "other-" + ns }},
		{"zones", func(f *fleet.CelldFleet) { f.Spec.Placement.Zones = []string{"us-east-1b"} }},
		{"serviceAccountName", func(f *fleet.CelldFleet) { f.Spec.ServiceAccountName = "other" }},
		{"env", func(f *fleet.CelldFleet) {
			value := "debug"
			f.Spec.Env = []fleet.FleetEnvVar{{Name: "CELLD_LOG", Value: &value}}
		}},
		{"telemetry", func(f *fleet.CelldFleet) {
			f.Spec.Telemetry = &fleet.TelemetrySpec{CollectorURL: "http://collector:4318", Egress: fleet.CollectorEgress{PodLabels: map[string]string{"app": "otel"}}}
		}},
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

func TestEnvtestTuningAdmission(t *testing.T) {
	c := envtestClient(t)
	ctx := t.Context()
	ns := fmt.Sprintf("tuning-%d", envtestSeq.Add(1))
	if err := c.Create(ctx, &corev1.Namespace{Name: ns}); err != nil {
		t.Fatal(err)
	}
	base := func(name string) *fleet.CelldFleet {
		return &fleet.CelldFleet{Name: name, Namespace: ns, Spec: fleet.CelldFleetSpec{
			Profile: "Bucket", ServiceAccountName: "runtime",
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

// External capacity mode is the /scale contract an autoscaler depends on: the
// selector it measures pods with, the spec count it writes, and the promise that
// the operator never writes that count back. The kind suite proves the same
// contract against a real HPA; this pins it against a real API server, where the
// CRD's own scale subresource, defaulting and CEL admission apply.

// A guarded claim deletion carries the UID it observed, so a delayed delete
// can never remove a replacement claim that reuses the member's name.
func TestEnvtestCleanupPreconditionsRejectReplacement(t *testing.T) {
	r, x := envtestSetup(t, "PersistentFleet")
	x.provision(t, r)
	old := initialClaims(x.fleet, workload(x.fleet, r.Options))[2]
	if err := r.Create(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	uid, rv := old.UID, old.ResourceVersion
	if err := r.Delete(t.Context(), old, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		t.Fatal(err)
	}
	// envtest has no PVC-protection controller. Simulate its release only
	// after observing the deletion and confirming this fixture has no pods.
	terminating := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(old), terminating); err != nil {
		t.Fatal(err)
	}
	terminating.Finalizers = nil
	if err := r.Update(t.Context(), terminating); err != nil {
		t.Fatal(err)
	}
	replacement := old.DeepCopy()
	replacement.UID = ""
	replacement.ResourceVersion = ""
	replacement.CreationTimestamp = metav1.Time{}
	if err := r.Create(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), replacement, client.Preconditions{UID: &uid, ResourceVersion: &rv}); !apierrors.IsConflict(err) {
		t.Fatalf("old cleanup deleted replacement: %v", err)
	}
}

// Bucket objects converge to the rendered spec. Against a real API server the
// server's defaulting must not make every reconcile rewrite the workload, and
// a template written by an earlier release migrates in place.
func TestEnvtestBucketConvergenceIsStable(t *testing.T) {
	for _, layout := range []string{"Deployment", "Ordered"} {
		t.Run(layout, func(t *testing.T) {
			r, x := envtestSetupWith(t, "Bucket", func(f *fleet.CelldFleet) { f.Spec.BucketWorkload = layout })
			reconcile(t, r, x.fleet)
			f := reconcile(t, r, x.fleet)
			w := emptyObject(workload(f, r.Options))
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
				t.Fatal(err)
			}
			if !matches(workload(f, r.Options), w) {
				t.Fatal("defaulted workload does not match its rendering")
			}
			// Simulate an 0022 template: launcher init container and command.
			legacy := f.DeepCopy()
			legacy.Spec.Profile = "PersistentFleet"
			strict := podTemplate(legacy, r.Options).Spec
			switch w := w.(type) {
			case *appsv1.Deployment:
				w.Spec.Template.Spec.InitContainers = strict.InitContainers
				w.Spec.Template.Spec.Containers[0].Command = strict.Containers[0].Command
				w.Spec.Template.Spec.Volumes = append(w.Spec.Template.Spec.Volumes, strict.Volumes...)
				w.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
			case *appsv1.StatefulSet:
				w.Spec.Template.Spec.InitContainers = strict.InitContainers
				w.Spec.Template.Spec.Containers[0].Command = strict.Containers[0].Command
				w.Spec.Template.Spec.Volumes = append(w.Spec.Template.Spec.Volumes, strict.Volumes...)
				w.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			}
			if err := r.Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, f)
			migrated := emptyObject(w)
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), migrated); err != nil {
				t.Fatal(err)
			}
			if !matches(workload(f, r.Options), migrated) {
				t.Fatal("legacy template not migrated")
			}
			version := migrated.GetResourceVersion()
			for range 3 {
				reconcile(t, r, f)
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), migrated); err != nil {
				t.Fatal(err)
			}
			if migrated.GetResourceVersion() != version {
				t.Fatal("steady-state reconcile rewrote the workload")
			}
		})
	}
}
