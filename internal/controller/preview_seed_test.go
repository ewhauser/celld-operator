package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func seededPreviewFixture() (*fleet.CelldPreview, *fleet.CelldPreviewPool, *fleet.CelldFleet, *fleet.CelldStorageReservation) {
	p := previewFixture()
	p.CreationTimestamp = metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	p.Spec.Seed = &fleet.PreviewSeedSpec{Source: "production", Objects: []fleet.PreviewObjectReference{{Class: "Cart", ID: "cart-123"}, {Class: "Customer", ID: "customer-456"}}}
	pool := previewPoolFixture(p)
	pool.Spec.ServiceAccountName = "runtime"
	source := fixture("source", "production-source", "Bucket")
	source.Namespace = "production"
	pool.Spec.Seeding = &fleet.PreviewSeedingSpec{Executor: "celld-snapshot-v1", Sources: []fleet.PreviewSeedSource{{Name: "production", FleetRef: fleet.SeedFleetReference{Namespace: source.Namespace, Name: source.Name, UID: string(source.UID)}}}}
	res := &fleet.CelldStorageReservation{Name: reservationName(source), Spec: fleetReservationSpec(source)}
	return p, pool, source, res
}

func seededSetup(t *testing.T) (*PreviewReconciler, *Reconciler, *fleet.CelldPreview) {
	t.Helper()
	p, pool, source, res := seededPreviewFixture()
	c := fake.NewClientBuilder().WithScheme(envtestScheme(t)).WithStatusSubresource(&fleet.CelldPreview{}, &fleet.CelldFleet{}, &fleet.CelldPreviewSeed{}).
		WithObjects(p, pool, source, res, &corev1.ServiceAccount{Name: "runtime", Namespace: p.Namespace}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if o.GetUID() == "" {
			o.SetUID(types.UID("created-" + o.GetName()))
		}
		return c.Create(ctx, o, opts...)
	}}).Build()
	return &PreviewReconciler{Client: c}, &Reconciler{Client: c, NetworkPolicyEnforced: true, Options: Options{OperatorNamespace: "celld-system", LauncherImage: fixtureLauncher}}, p
}

func getSeedAndFleet(t *testing.T, c client.Client, p *fleet.CelldPreview) (*fleet.CelldPreviewSeed, *fleet.CelldFleet) {
	t.Helper()
	s := &fleet.CelldPreviewSeed{}
	f := &fleet.CelldFleet{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: p.Namespace, Name: seedName(p)}, s); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: p.Namespace, Name: previewName(p)}, f); err != nil {
		t.Fatal(err)
	}
	return s, f
}

func completeSeed(t *testing.T, c client.Client, s *fleet.CelldPreviewSeed, f *fleet.CelldFleet) {
	t.Helper()
	s.Status.Phase = "Running"
	s.Status.ExecutorID = "execution-one"
	s.Status.TargetFleetUID = string(f.UID)
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Status.Manifest = &fleet.PreviewSnapshotManifest{}
	for _, o := range s.Spec.Request.Selection.Objects {
		s.Status.Manifest.Objects = append(s.Status.Manifest.Objects, fleet.PreviewObjectSnapshot{PreviewObjectReference: o, SnapshotID: "snapshot-" + o.ID, SourceVersion: "tx-42", Digest: strings.Repeat("a", 64)})
	}
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Status.Phase = "Succeeded"
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}

func assertSeedGated(t *testing.T, r *Reconciler, f *fleet.CelldFleet) {
	t.Helper()
	got := reconcile(t, r, f)
	reason(t, got, "SeedInitializing")
	if condition := meta.FindStatusCondition(got.Status.Conditions, "RoutingReady"); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatal("seed route is ready")
	}
	for _, o := range []client.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &networkingv1.Ingress{}} {
		name := f.Name
		if _, ok := o.(*networkingv1.Ingress); ok {
			name += "-routing"
		}
		if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: name}, o); !apierrors.IsNotFound(err) {
			t.Fatalf("resource created before seeding: %T %v", o, err)
		}
	}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if res.Spec.FleetUID != string(f.UID) || res.Annotations[attemptAnnotation] != "" {
		t.Fatal("seed lacks exclusive destination or issued startup early")
	}
}

func TestPreviewSeedGatesAllObjectsAndRetainsReceipt(t *testing.T) {
	pr, fr, p := seededSetup(t)
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Initializing" || got.Status.SeedName == "" || got.Status.URL == "" {
		t.Fatalf("missing initialization status: %+v", got.Status)
	}
	s, f := getSeedAndFleet(t, pr.Client, p)
	if len(s.OwnerReferences) != 0 || s.Spec.Request.Selection.Alarms != "Clear" || len(s.Spec.Request.Selection.Objects) != 2 || f.Spec.Storage.Initialization.UID != string(s.UID) {
		t.Fatal("seed request not retained/bound")
	}
	assertSeedGated(t, fr, f)
	completeSeed(t, pr.Client, s, f)
	// Corrupt completions bypassing admission must also fail closed at runtime.
	for _, mutate := range []func(*fleet.CelldPreviewSeed){
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest.Objects = s.Status.Manifest.Objects[:1] },
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest.Objects[1] = s.Status.Manifest.Objects[0] },
		func(s *fleet.CelldPreviewSeed) { s.Status.TargetFleetUID = "other" },
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest.Objects[0].Digest = "bad" },
	} {
		broken := s.DeepCopy()
		mutate(broken)
		if _, err := completedSeedReceipt(broken, f); err == nil {
			t.Fatal("invalid seed completion accepted")
		}
	}
	gotFleet := reconcile(t, fr, f)
	if meta.FindStatusCondition(gotFleet.Status.Conditions, "Ready").Reason == "SeedInitializing" {
		t.Fatalf("completed seed remains gated: %+v", gotFleet.Status)
	}
	workload := &appsv1.StatefulSet{}
	if err := fr.Get(t.Context(), client.ObjectKeyFromObject(f), workload); err != nil {
		t.Fatal(err)
	}
	res := &fleet.CelldStorageReservation{}
	if err := fr.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if !seedReceiptReady(f, res) {
		t.Fatal("completion receipt not persisted")
	}
	workloadUID := workload.UID
	// Lost projection does not reinitialize; a code revision keeps the same seed.
	got = previewReconcile(t, pr, p)
	got.Spec.Revision = "another-commit"
	got.Status = fleet.CelldPreviewStatus{}
	if err := pr.Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if err := pr.Status().Update(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	got = previewReconcile(t, pr, p)
	if got.Status.SeedPhase != "Succeeded" || got.Status.Conditions[0].Reason == "FleetDrift" {
		t.Fatal("status loss/revision reran seed")
	}
	if err := fr.Delete(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	// Fleet lifecycle keeps the durable completion even if the retained request is lost.
	reconcile(t, fr, f)
	if err := fr.Get(t.Context(), client.ObjectKeyFromObject(f), workload); err != nil || workload.UID != workloadUID {
		t.Fatal("completed fleet recreated after seed loss")
	}
	got = previewReconcile(t, pr, p)
	if got.Status.Conditions[0].Reason != "SeedBlocked" {
		t.Fatal("lost request silently recreated")
	}
}

func TestPreviewSeedAuthorizationAndReplacement(t *testing.T) {
	for _, change := range []string{"unapproved", "replaced-source", "reservation", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			pr, _, p := seededSetup(t)
			switch change {
			case "unapproved":
				p.Spec.Seed.Source = "unapproved"
				if err := pr.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				p.Spec.Seed.Objects[1] = p.Spec.Seed.Objects[0]
				if err := pr.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			default:
				source := &fleet.CelldFleet{}
				if err := pr.Get(t.Context(), client.ObjectKey{Namespace: "production", Name: "source"}, source); err != nil {
					t.Fatal(err)
				}
				if change == "replaced-source" {
					source.UID = "replacement"
					if err := pr.Update(t.Context(), source); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := pr.Delete(t.Context(), &fleet.CelldStorageReservation{Name: reservationName(source)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := previewReconcile(t, pr, p)
			if got.Status.Phase != "Blocked" {
				t.Fatal("unauthorized/invalid clone admitted")
			}
			seeds := &fleet.CelldPreviewSeedList{}
			if err := pr.List(t.Context(), seeds); err != nil {
				t.Fatal(err)
			}
			if len(seeds.Items) != 0 {
				t.Fatal("unauthorized seed created")
			}
		})
	}
	pr, _, p := seededSetup(t)
	previewReconcile(t, pr, p)
	s, _ := getSeedAndFleet(t, pr.Client, p)
	if err := pr.Delete(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.UID = "replacement"
	s.ResourceVersion = ""
	if err := pr.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Blocked" {
		t.Fatal("replacement seed adopted")
	}
}

func TestPreviewSeedCancellation(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "running"}[running], func(t *testing.T) {
			pr, fr, p := seededSetup(t)
			previewReconcile(t, pr, p)
			s, f := getSeedAndFleet(t, pr.Client, p)
			assertSeedGated(t, fr, f)
			if running {
				s.Status = fleet.CelldPreviewSeedStatus{Phase: "Running", ExecutorID: "execution-one", TargetFleetUID: string(f.UID)}
				if err := pr.Status().Update(t.Context(), s); err != nil {
					t.Fatal(err)
				}
			}
			pr.now = func() time.Time { return p.CreationTimestamp.Add(2 * time.Hour) }
			got := previewReconcile(t, pr, p)
			if err := pr.Get(t.Context(), client.ObjectKeyFromObject(s), s); err != nil {
				t.Fatal(err)
			}
			if !s.Spec.Canceled {
				t.Fatal("cancellation not persisted")
			}
			if running {
				if got.Status.Conditions[0].Reason != "SeedCanceling" {
					t.Fatal("did not wait for executor")
				}
				if err := pr.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil || !f.DeletionTimestamp.IsZero() {
					t.Fatal("child deleted during active import")
				}
				s.Status.Phase = "Canceled"
				if err := pr.Status().Update(t.Context(), s); err != nil {
					t.Fatal(err)
				}
				previewReconcile(t, pr, p)
			}
			if _, err := fr.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
				t.Fatal(err)
			}
			if err := fr.Get(t.Context(), client.ObjectKeyFromObject(f), f); !apierrors.IsNotFound(err) {
				t.Fatalf("unstarted child not removed: %v", err)
			}
			got = previewReconcile(t, pr, p)
			if got.Status.Phase != "Expired" {
				t.Fatalf("preview did not expire: %+v", got.Status)
			}
			res := &fleet.CelldStorageReservation{}
			if err := fr.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			if res.Annotations[seedGate] != seedGateCanceled {
				t.Fatal("canceled destination not retained")
			}
		})
	}
}

func TestPreviewSeedLostCreationResponse(t *testing.T) {
	pr, _, p := seededSetup(t)
	base := pr.Client
	pr.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if _, ok := o.(*fleet.CelldPreviewSeed); ok {
			return errors.New("unknown create outcome")
		}
		return c.Create(ctx, o, opts...)
	}})
	previewReconcile(t, pr, p)
	pr.Client = base
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Blocked" || !strings.Contains(got.Status.Conditions[0].Message, "creation intent") {
		t.Fatal("ambiguous seed creation retried")
	}
}

func envSeededPreview(t *testing.T) (client.Client, *fleet.CelldPreview, *fleet.CelldPreviewSeed, *fleet.CelldFleet) {
	t.Helper()
	c := envtestClient(t)
	ns := &corev1.Namespace{GenerateName: "seed-preview-"}
	if err := c.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	p, pool, source, _ := seededPreviewFixture()
	source.Namespace = ns.Name
	source.UID = ""
	source.ResourceVersion = ""
	source.Spec.Storage.Bucket = ns.Name + "-source"
	if err := c.Create(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), &fleet.CelldStorageReservation{Name: reservationName(source), Spec: fleetReservationSpec(source)}); err != nil {
		t.Fatal(err)
	}
	// Each envtest run needs a distinct source bucket as well as target bucket.
	pool.Namespace = ns.Name
	pool.UID = ""
	pool.Spec.Storage.Bucket = ns.Name + "-target"
	pool.Spec.Seeding.Sources[0].FleetRef = fleet.SeedFleetReference{Namespace: source.Namespace, Name: source.Name, UID: string(source.UID)}
	if err := c.Create(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	p.Namespace = ns.Name
	p.UID = ""
	p.CreationTimestamp = metav1.Time{}
	if err := c.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), &corev1.ServiceAccount{Name: "runtime", Namespace: ns.Name}); err != nil {
		t.Fatal(err)
	}
	p = previewReconcile(t, &PreviewReconciler{Client: c}, p)
	s, f := getSeedAndFleet(t, c, p)
	return c, p, s, f
}

func TestEnvtestPreviewSeedAdmissionAndCompletion(t *testing.T) {
	c, p, s, f := envSeededPreview(t)
	for _, mutate := range []func(*fleet.CelldPreview){
		func(p *fleet.CelldPreview) { p.Spec.Seed = nil },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Objects = p.Spec.Seed.Objects[:1] },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Source = "other" },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Alarms = "Preserve" },
	} {
		changed := p.DeepCopy()
		mutate(changed)
		if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("immutable seed selection changed: %v", err)
		}
	}
	for _, mutate := range []func(*fleet.CelldPreview){
		func(p *fleet.CelldPreview) { p.Spec.Seed.Objects = nil },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Objects[1] = p.Spec.Seed.Objects[0] },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Objects[0].ID = "../other" },
		func(p *fleet.CelldPreview) { p.Spec.Seed.Objects = make([]fleet.PreviewObjectReference, 101) },
	} {
		changed := p.DeepCopy()
		changed.Name = "invalid"
		changed.ResourceVersion = ""
		changed.UID = ""
		mutate(changed)
		if err := c.Create(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid seed selection admitted: %v", err)
		}
	}
	empty := p.DeepCopy()
	empty.Name = "empty"
	empty.UID = ""
	empty.ResourceVersion = ""
	empty.Spec.Seed = nil
	if err := c.Create(t.Context(), empty); err != nil {
		t.Fatal(err)
	}
	empty.Spec.Seed = p.Spec.Seed.DeepCopy()
	if err := c.Update(t.Context(), empty); !apierrors.IsInvalid(err) {
		t.Fatalf("late seeding admitted: %v", err)
	}
	fr := &Reconciler{Client: c, NetworkPolicyEnforced: true, Options: Options{OperatorNamespace: "celld-system", LauncherImage: fixtureLauncher}}
	assertSeedGated(t, fr, f)
	completeSeed(t, c, s, f)
	for _, mutate := range []func(*fleet.CelldPreviewSeed){
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest.Objects[0].SnapshotID = "new-snapshot" },
		func(s *fleet.CelldPreviewSeed) { s.Status.Phase = "Running" },
		func(s *fleet.CelldPreviewSeed) { s.Status = fleet.CelldPreviewSeedStatus{} },
	} {
		changed := s.DeepCopy()
		mutate(changed)
		if err := c.Status().Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("terminal seed result changed: %v", err)
		}
	}
	changed := s.DeepCopy()
	changed.Spec.Request.Selection.Objects = changed.Spec.Request.Selection.Objects[:1]
	if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
		t.Fatalf("seed request changed: %v", err)
	}
	reconcile(t, fr, f)
	workload := &appsv1.StatefulSet{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(f), workload); err != nil {
		t.Fatal(err)
	}
}

func TestEnvtestPreviewSeedClaimCancellationCAS(t *testing.T) {
	c, _, s, f := envSeededPreview(t)
	stale := s.DeepCopy()
	stopped, err := cancelSeed(t.Context(), c, s)
	if err != nil || !stopped {
		t.Fatalf("pending cancellation: %v", err)
	}
	stale.Status = fleet.CelldPreviewSeedStatus{Phase: "Running", TargetFleetUID: string(f.UID), ExecutorID: "late-worker"}
	if err := c.Status().Update(t.Context(), stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale worker acquired canceled request: %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(s), stale); err != nil {
		t.Fatal(err)
	}
	stale.Status.Phase = "Running"
	stale.Status.TargetFleetUID = string(f.UID)
	stale.Status.ExecutorID = "late-worker"
	if err := c.Status().Update(t.Context(), stale); !apierrors.IsInvalid(err) {
		t.Fatalf("canceled request resurrected: %v", err)
	}
	changed := s.DeepCopy()
	changed.Spec.Canceled = false
	if err := c.Update(t.Context(), changed); !apierrors.IsInvalid(err) {
		t.Fatalf("cancellation reversed: %v", err)
	}
}

func TestEnvtestPreviewSeedPinnedManifestAndRunningCancellation(t *testing.T) {
	c, _, s, f := envSeededPreview(t)
	s.Status = fleet.CelldPreviewSeedStatus{Phase: "Running", TargetFleetUID: string(f.UID), ExecutorID: "worker-one"}
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Status.Manifest = &fleet.PreviewSnapshotManifest{Objects: []fleet.PreviewObjectSnapshot{{PreviewObjectReference: s.Spec.Request.Selection.Objects[0], SnapshotID: "snapshot", SourceVersion: "version", Digest: strings.Repeat("a", 64)}}}
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fleet.CelldPreviewSeed){
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest = nil },
		func(s *fleet.CelldPreviewSeed) { s.Status.Manifest.Objects[0].SnapshotID = "other" },
		func(s *fleet.CelldPreviewSeed) { s.Status.TargetFleetUID = "other" },
		func(s *fleet.CelldPreviewSeed) { s.Status.ExecutorID = "other" },
		func(s *fleet.CelldPreviewSeed) { s.Status.Phase = "Pending" },
	} {
		changed := s.DeepCopy()
		mutate(changed)
		if err := c.Status().Update(t.Context(), changed); !apierrors.IsInvalid(err) {
			t.Fatalf("claim or captured manifest replaced: %v", err)
		}
	}
	stopped, err := cancelSeed(t.Context(), c, s)
	if err != nil || stopped {
		t.Fatalf("running cancellation did not wait: %v", err)
	}
	changed := s.DeepCopy()
	changed.Status.Phase = "Succeeded"
	if err := c.Status().Update(t.Context(), changed); !apierrors.IsInvalid(err) {
		t.Fatalf("success accepted after cancellation: %v", err)
	}
	s.Status.Phase = "Canceled"
	if err := c.Status().Update(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewSeedStartupCancellationRace(t *testing.T) {
	pr, fr, p := seededSetup(t)
	previewReconcile(t, pr, p)
	s, f := getSeedAndFleet(t, pr.Client, p)
	assertSeedGated(t, fr, f)
	completeSeed(t, pr.Client, s, f)
	stale := &fleet.CelldStorageReservation{}
	if err := fr.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, stale); err != nil {
		t.Fatal(err)
	}
	current := stale.DeepCopy()
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[seedGate] = seedGateCanceled
	if err := fr.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if ok, err := fr.gateSeed(t.Context(), f, stale); ok || !apierrors.IsConflict(err) {
		t.Fatalf("stale startup bypassed cancellation: %v %v", ok, err)
	}
	assertSeedGated(t, fr, f)
}

func TestPreviewSeedSurfacesProvisioningBlock(t *testing.T) {
	pr, fr, p := seededSetup(t)
	previewReconcile(t, pr, p)
	_, f := getSeedAndFleet(t, pr.Client, p)
	if err := pr.Delete(t.Context(), &corev1.ServiceAccount{Name: "runtime", Namespace: p.Namespace}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, fr, f)
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Blocked" || got.Status.Conditions[0].Reason != "ServiceAccountMissing" {
		t.Fatalf("hidden provisioning error: %+v", got.Status)
	}
}

func TestPreviewSeedCancellationBeforeReservation(t *testing.T) {
	pr, fr, p := seededSetup(t)
	previewReconcile(t, pr, p)
	_, f := getSeedAndFleet(t, pr.Client, p)
	pr.now = func() time.Time { return p.CreationTimestamp.Add(2 * time.Hour) }
	previewReconcile(t, pr, p)
	if _, err := fr.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
		t.Fatal(err)
	}
	if err := pr.Get(t.Context(), client.ObjectKeyFromObject(f), f); !apierrors.IsNotFound(err) {
		t.Fatalf("unstarted fleet not deleted: %v", err)
	}
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Expired" {
		t.Fatalf("early expiry stranded: %+v", got.Status)
	}
	res := &fleet.CelldStorageReservation{}
	if err := fr.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if res.Annotations[seedGate] != seedGateCanceled {
		t.Fatal("early cancellation lost its destination tombstone")
	}
}

func TestPreviewSeedCreatedWithLostResponse(t *testing.T) {
	pr, _, p := seededSetup(t)
	base := pr.Client
	pr.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, o, opts...); err != nil {
			return err
		}
		if _, ok := o.(*fleet.CelldPreviewSeed); ok {
			return errors.New("lost accepted response")
		}
		return nil
	}})
	previewReconcile(t, pr, p)
	pr.Client = base
	got := previewReconcile(t, pr, p)
	if got.Status.Phase != "Initializing" {
		t.Fatalf("accepted request was not resumed: %+v", got.Status)
	}
	s, f := getSeedAndFleet(t, pr.Client, p)
	if string(s.UID) != f.Spec.Storage.Initialization.UID || got.Annotations[previewSeedCreated] != string(s.UID) {
		t.Fatal("lost response changed operation identity")
	}
}

func TestPreviewSeedReadyGateCannotShortcutDeletion(t *testing.T) {
	pr, fr, p := seededSetup(t)
	previewReconcile(t, pr, p)
	s, f := getSeedAndFleet(t, pr.Client, p)
	assertSeedGated(t, fr, f)
	completeSeed(t, pr.Client, s, f)
	res := &fleet.CelldStorageReservation{}
	if err := fr.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	if ok, err := fr.gateSeed(t.Context(), f, res); !ok || err != nil {
		t.Fatalf("startup gate: %v", err)
	}
	if handled, err := fr.deleteUninitializedFleet(t.Context(), f); handled || err != nil {
		t.Fatalf("ready gate bypassed lifecycle: %v", err)
	}
	if err := pr.Get(t.Context(), client.ObjectKeyFromObject(f), f); err != nil || len(f.Finalizers) == 0 {
		t.Fatal("ready fleet lost safety finalizer")
	}
	res.Spec.FleetUID = "foreign"
	if seedReceiptReady(f, res) {
		t.Fatal("foreign reservation opened startup gate")
	}
}
