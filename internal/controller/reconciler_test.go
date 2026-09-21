package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(name, bucket, profile string) *fleet.CelldFleet {
	f := &fleet.CelldFleet{
		Name:      name,
		Namespace: "fleets",
		UID:       types.UID(name + "-uid"),
		Spec: fleet.CelldFleetSpec{
			Qualification:      "Experimental",
			RuntimeImage:       fixtureRuntime,
			Profile:            profile,
			ServiceAccountName: "runtime",
			Storage:            fleet.StorageSpec{Bucket: bucket, Region: "us-east-1"},
			Placement:          fleet.PlacementSpec{AZCount: 2, Zones: []string{"us-east-1a", "us-east-1b"}},
		},
	}
	f.Default()
	if profile == "PersistentFleet" {
		f.Spec.Storage.StorageClassName = "disposable"
	}
	return f
}
func setup(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := fleet.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, &corev1.ServiceAccount{Name: "runtime", Namespace: "fleets"}, &storagev1.StorageClass{
		Name:              "disposable",
		Provisioner:       "ebs.csi.aws.com",
		ReclaimPolicy:     ptr.To(corev1.PersistentVolumeReclaimDelete),
		VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
	})
	return &Reconciler{
		// The production client is direct, so the API server serves the
		// spec.nodeName field selector natively; the fake client only honors
		// selectors backed by a registered index.
		Client: fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&fleet.CelldFleet{}, &appsv1.Deployment{}, &appsv1.StatefulSet{}).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("created-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build(),
		Options:               Options{OperatorNamespace: "celld-system", LauncherImage: fixtureLauncher},
		NetworkPolicyEnforced: true,
	}
}
func reconcile(t *testing.T, r *Reconciler, f *fleet.CelldFleet) *fleet.CelldFleet {
	t.Helper()
	ctx := t.Context()
	original := r.Client
	r.Client = auditManifestClient(t, original)
	defer func() { r.Client = original }()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
		t.Fatal(err)
	}
	got := &fleet.CelldFleet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatal(err)
	}
	return got
}
func reason(t *testing.T, f *fleet.CelldFleet, want string) {
	t.Helper()
	c := meta.FindStatusCondition(f.Status.Conditions, "Ready")
	if c == nil || c.Reason != want {
		t.Fatalf("want %s, got %+v", want, c)
	}
}
func TestProvisionProfilesAndIsolation(t *testing.T) {
	a := fixture("alpha", "bucket-alpha", "Bucket")
	b := fixture("beta", "bucket-beta", "PersistentFleet")
	r := setup(t, a, b)
	for _, f := range []*fleet.CelldFleet{a, b} {
		reason(t, reconcile(t, r, f), "Provisioning")
		reason(t, reconcile(t, r, f), "Provisioning")
		p := &networkingv1.NetworkPolicy{}
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
			t.Fatal(err)
		}
		if len(p.Spec.Ingress) != 3 || len(p.Spec.Ingress[0].From) != 2 || p.Spec.Ingress[0].From[0].PodSelector.MatchLabels[FleetLabel] != string(f.UID) || p.Spec.Ingress[0].From[1].NamespaceSelector == nil {
			t.Fatal("peer isolation is not constrained")
		}
		w := workload(f, r.Options)
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
			t.Fatal(err)
		}
		if len(w.GetOwnerReferences()) != 0 {
			t.Fatal("workload may be garbage collected")
		}
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(b), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType || sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType || sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatal("unsafe persistent lifecycle")
	}
	container := sts.Spec.Template.Spec.Containers[0]
	if container.LivenessProbe != nil || container.StartupProbe != nil || container.ReadinessProbe.HTTPGet.Path != "/.well-known/celld/health" || len(container.Command) != 1 || container.Command[0] != "/launcher/celld-launcher" {
		t.Fatal("runtime startup/readiness contract bypassed")
	}
	spread := sts.Spec.Template.Spec.TopologySpreadConstraints[0]
	if spread.MinDomains == nil || *spread.MinDomains != 2 || spread.WhenUnsatisfiable != corev1.DoNotSchedule {
		t.Fatal("strict AZ placement missing")
	}
	b.Spec.Placement.Mode = "Relaxed"
	relaxed := podTemplate(b, r.Options).Spec.TopologySpreadConstraints[0]
	if relaxed.MinDomains != nil || relaxed.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Fatal("invalid relaxed placement")
	}
}
func TestStorageConflictAcrossNamespaces(t *testing.T) {
	a := fixture("alpha", "shared-bucket", "Bucket")
	b := fixture("beta", "shared-bucket", "Bucket")
	b.Namespace = "other"
	r := setup(t, a, b, &corev1.ServiceAccount{Name: "runtime", Namespace: "other"})
	reason(t, reconcile(t, r, a), "Provisioning")
	reason(t, reconcile(t, r, b), "StorageScopeConflict")
	list := &appsv1.DeploymentList{}
	if err := r.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatal("conflicting fleet provisioned")
	}
}
func TestConcurrentReservation(t *testing.T) {
	a := fixture("alpha", "shared-bucket", "Bucket")
	b := fixture("beta", "shared-bucket", "Bucket")
	r := setup(t, a, b)
	var wg sync.WaitGroup
	for _, f := range []*fleet.CelldFleet{a, b} {
		wg.Go(func() {
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	list := &appsv1.DeploymentList{}
	if err := r.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d writers", len(list.Items))
	}
}
func TestNoUnsafeMutationsOrRecreation(t *testing.T) {
	for _, change := range []string{"scale-in", "profile", "missing", "drift", "delete"} {
		t.Run(change, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "Bucket")
			r := setup(t, f)
			f = reconcile(t, r, f)
			switch change {
			case "scale-in":
				f.Spec.Replicas = 2
			case "scale-out":
				f.Spec.Replicas = 4
			case "profile":
				f.Spec.Profile = "PersistentFleet"
				f.Spec.Storage.StorageClassName = "disposable"
			case "missing":
				if err := r.Delete(t.Context(), &appsv1.Deployment{Name: f.Name, Namespace: f.Namespace}); err != nil {
					t.Fatal(err)
				}
			case "drift":
				d := &appsv1.Deployment{}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), d); err != nil {
					t.Fatal(err)
				}
				d.Spec.Template.Spec.Containers[0].Image = "unexpected"
				if err := r.Update(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := r.Delete(t.Context(), f); err != nil {
					t.Fatal(err)
				}
			}
			if change == "scale-in" || change == "scale-out" || change == "profile" {
				if err := r.Update(t.Context(), f); err != nil {
					t.Fatal(err)
				}
			}
			got := reconcile(t, r, f)
			c := meta.FindStatusCondition(got.Status.Conditions, "Blocked")
			if c == nil || c.Status != metav1.ConditionTrue {
				t.Fatal("unsafe operation not blocked")
			}
			ds := &appsv1.DeploymentList{}
			if err := r.List(t.Context(), ds); err != nil {
				t.Fatal(err)
			}
			if change == "missing" {
				if len(ds.Items) != 0 {
					t.Fatal("missing workload recreated")
				}
			} else if len(ds.Items) != 1 || *ds.Items[0].Spec.Replicas != 3 {
				t.Fatal("workload disrupted")
			}
		})
	}
}
func TestCreationCrashFailsClosed(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return errors.New("injected create failure")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err == nil {
		t.Fatal("failure not injected")
	}
	r.Client = base
	reason(t, reconcile(t, r, f), "LifecycleBlocked")
}
func TestMissingPrerequisitesAndIsolationDrift(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	r.NetworkPolicyEnforced = false
	reason(t, reconcile(t, r, f), "IsolationUnverified")
	r.NetworkPolicyEnforced = true
	reconcile(t, r, f)
	p := &networkingv1.NetworkPolicy{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Ingress = append(p.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{})
	if err := r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, r, f), "InfrastructureBlocked")
}

func TestDependenciesBlockBeforeReservation(t *testing.T) {
	for _, missing := range []string{"serviceaccount", "storageclass", "reclaim-policy"} {
		t.Run(missing, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "PersistentFleet")
			r := setup(t, f)
			switch missing {
			case "serviceaccount":
				if err := r.Delete(t.Context(), &corev1.ServiceAccount{Name: "runtime", Namespace: "fleets"}); err != nil {
					t.Fatal(err)
				}
			case "storageclass":
				if err := r.Delete(t.Context(), &storagev1.StorageClass{Name: "disposable"}); err != nil {
					t.Fatal(err)
				}
			case "reclaim-policy":
				sc := &storagev1.StorageClass{}
				if err := r.Get(t.Context(), types.NamespacedName{Name: "disposable"}, sc); err != nil {
					t.Fatal(err)
				}
				sc.ReclaimPolicy = new(corev1.PersistentVolumeReclaimRetain)
				if err := r.Update(t.Context(), sc); err != nil {
					t.Fatal(err)
				}
			}
			got := reconcile(t, r, f)
			if !meta.IsStatusConditionTrue(got.Status.Conditions, "Blocked") {
				t.Fatal("dependency failure not reported")
			}
			rs := &fleet.CelldStorageReservationList{}
			if err := r.List(t.Context(), rs); err != nil {
				t.Fatal(err)
			}
			if len(rs.Items) != 0 {
				t.Fatal("reserved storage before dependencies validated")
			}
		})
	}
}

func TestReservationCannotBeReclaimedByNewUID(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	reconcile(t, r, f)
	replacement := f.DeepCopy()
	replacement.UID = "replacement-uid"
	rs := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, rs); err != nil {
		t.Fatal(err)
	}
	other := setup(t, replacement, rs)
	reason(t, reconcile(t, other, replacement), "StorageScopeConflict")
}

// Current operation state that cannot be read is reported as unreadable, with the load error,
// not as a storage scope conflict: the reservation still binds exactly this
// fleet, and the reason an operator sees has to name the actual failure.
func TestUnreadableOperationReportsInvalidState(t *testing.T) {
	r, f := lifecycleSetup(t, "Bucket")
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, res); err != nil {
		t.Fatal(err)
	}
	res.Annotations[stateKey] = `{"Version":99,"Initial":3,"Applied":3}`
	if err := r.Update(t.Context(), res); err != nil {
		t.Fatal(err)
	}
	_, want := readState(res)
	if want == nil {
		t.Fatal("fixture operation state is still readable")
	}
	got := reconcile(t, r, f)
	reason(t, got, "OperationInvalid")
	if c := meta.FindStatusCondition(got.Status.Conditions, "Ready"); c.Message != want.Error() {
		t.Fatalf("message %q does not carry the load error %q", c.Message, want)
	}
	after := &fleet.CelldStorageReservation{}
	if err := r.Get(t.Context(), types.NamespacedName{Name: reservationName(f)}, after); err != nil {
		t.Fatal(err)
	}
	if after.Annotations[stateKey] != res.Annotations[stateKey] || after.ResourceVersion != res.ResourceVersion {
		t.Fatal("unreadable operation state was rewritten; evidence must be retained for review")
	}
}

func TestReadinessRequiresObservedWorkload(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	reconcile(t, r, f)
	d := &appsv1.Deployment{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), d); err != nil {
		t.Fatal(err)
	}
	d.Generation = 2
	if err := r.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	d.Status.ReadyReplicas = 3
	d.Status.ObservedGeneration = 1
	if err := r.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	reason(t, reconcile(t, r, f), "Provisioning")
	d.Status.ObservedGeneration = 2
	if err := r.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	got := reconcile(t, r, f)
	reason(t, got, "Provisioned")
	if !meta.IsStatusConditionFalse(got.Status.Conditions, "ProductionQualified") || !meta.IsStatusConditionTrue(got.Status.Conditions, "LifecycleBlocked") {
		t.Fatal("readiness erased safety boundary")
	}
	if got.Status.Reservation != reservationName(f) {
		t.Fatal("reservation missing from status")
	}
}

func TestAPIDefaultedProbeIsNotDrift(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	want := workload(f, Options{}).(*appsv1.Deployment)
	got := want.DeepCopy()
	got.Spec.Template.Spec.Containers[0].ReadinessProbe.SuccessThreshold = 1
	if !matches(want, got) {
		t.Fatal("Kubernetes defaulted readiness successThreshold must not block infrastructure")
	}
}

func TestExistingPVCBlocksInitialProvisioning(t *testing.T) {
	for _, owner := range []string{"previous-fleet-uid", "alpha-uid", ""} {
		t.Run(owner, func(t *testing.T) {
			f := fixture("alpha", "new-bucket", "PersistentFleet")
			pvc := &corev1.PersistentVolumeClaim{Name: "data-alpha-0", Namespace: f.Namespace, Labels: map[string]string{FleetLabel: owner}}
			r := setup(t, f, pvc)
			reason(t, reconcile(t, r, f), "StorageIdentityConflict")
			sts := &appsv1.StatefulSetList{}
			if err := r.List(t.Context(), sts); err != nil {
				t.Fatal(err)
			}
			if len(sts.Items) != 0 {
				t.Fatal("created a workload against an unverified existing claim")
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc); err != nil {
				t.Fatal("existing PVC was removed", err)
			}
		})
	}
}

func TestRejectAdditionalWorkloadConfiguration(t *testing.T) {
	mutations := map[string]func(*corev1.PodSpec){
		"liveness": func(p *corev1.PodSpec) {
			p.Containers[0].LivenessProbe = &corev1.Probe{Exec: &corev1.ExecAction{Command: []string{"false"}}}
		},
		"bucket override": func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "CELLD_BUCKET", Value: "s3://other-fleet"})
		},
		"sidecar": func(p *corev1.PodSpec) {
			p.Containers = append(p.Containers, corev1.Container{Name: "extra", Image: "unexpected"})
		},
		"init container": func(p *corev1.PodSpec) { p.InitContainers = []corev1.Container{{Name: "extra", Image: "unexpected"}} },
		"volume": func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "extra", HostPath: &corev1.HostPathVolumeSource{Path: "/"}})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "Bucket")
			r := setup(t, f)
			reconcile(t, r, f)
			d := &appsv1.Deployment{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(f), d); err != nil {
				t.Fatal(err)
			}
			mutate(&d.Spec.Template.Spec)
			if err := r.Update(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			reason(t, reconcile(t, r, f), "LifecycleBlocked")
		})
	}
}

func TestConcurrentFinalizerIsPreserved(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	r := setup(t, f)
	once := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*fleet.CelldFleet); ok && !once {
			once = true
			other := &fleet.CelldFleet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), other); err != nil {
				return err
			}
			other.Finalizers = append(other.Finalizers, "another.example.com/protect")
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.Patch(ctx, obj, p, opts...)
	}})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected resource-version conflict, got %v", err)
	}
	got := reconcile(t, r, f)
	if !slices.Contains(got.Finalizers, "another.example.com/protect") || !slices.Contains(got.Finalizers, Finalizer) {
		t.Fatalf("lost a finalizer: %v", got.Finalizers)
	}
}

func TestPVCClaimCreationRaceAndPartialFailure(t *testing.T) {
	for _, mode := range []string{"race", "partial failure"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture("alpha", "new-bucket", "PersistentFleet")
			r := setup(t, f)
			base := r.Client
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Name == "data-alpha-1" {
					if mode == "partial failure" {
						return errors.New("injected storage failure")
					}
					foreign := pvc.DeepCopy()
					foreign.Labels = map[string]string{FleetLabel: "other-fleet"}
					if err := c.Create(ctx, foreign); err != nil {
						return err
					}
				}
				return c.Create(ctx, obj, opts...)
			}})
			reason(t, reconcile(t, r, f), "StorageIdentityConflict")
			r.Client = base
			reason(t, reconcile(t, r, f), "LifecycleBlocked")
			sts := &appsv1.StatefulSetList{}
			if err := r.List(t.Context(), sts); err != nil {
				t.Fatal(err)
			}
			if len(sts.Items) != 0 {
				t.Fatal("workload created after incomplete exclusive PVC allocation")
			}
			claim := &corev1.PersistentVolumeClaim{}
			if err := r.Get(t.Context(), types.NamespacedName{Namespace: f.Namespace, Name: "data-alpha-0"}, claim); err != nil {
				t.Fatal(err)
			}
			if len(claim.OwnerReferences) != 0 || claim.Labels[FleetLabel] != string(f.UID) || claim.Annotations["celld.eric.dev/storage-reservation"] != reservationName(f) {
				t.Fatal("initial PVC is not independently retained and bound to fleet reservation")
			}
		})
	}
}

func TestComparisonAllowsOnlyKnownDefaults(t *testing.T) {
	f := fixture("alpha", "bucket-alpha", "Bucket")
	want := workload(f, Options{}).(*appsv1.Deployment)
	got := want.DeepCopy()
	got.Spec.RevisionHistoryLimit = new(int32(10))
	got.Spec.ProgressDeadlineSeconds = new(int32(600))
	p := &got.Spec.Template.Spec
	p.DNSPolicy = corev1.DNSClusterFirst
	p.RestartPolicy = corev1.RestartPolicyAlways
	p.SchedulerName = corev1.DefaultSchedulerName
	p.DeprecatedServiceAccount = f.Spec.ServiceAccountName
	c := &p.Containers[0]
	c.TerminationMessagePath = corev1.TerminationMessagePathDefault
	c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	for i := range c.Ports {
		c.Ports[i].Protocol = corev1.ProtocolTCP
	}
	for i := range c.Env {
		if c.Env[i].ValueFrom != nil {
			c.Env[i].ValueFrom.FieldRef.APIVersion = "v1"
		}
	}
	c.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTP
	if !matches(want, got) {
		t.Fatal("known API defaults considered drift")
	}
	p.SchedulerName = "different-scheduler"
	if matches(want, got) {
		t.Fatal("nondefault scheduler accepted")
	}
	for _, obj := range prerequisites(f, Options{OperatorNamespace: "celld-system", LauncherImage: fixtureLauncher}) {
		s, ok := obj.(*corev1.Service)
		if !ok {
			continue
		}
		server := s.DeepCopy()
		if server.Spec.ClusterIP != "None" {
			server.Spec.ClusterIP = "10.96.0.42"
		}
		server.Spec.ClusterIPs = []string{server.Spec.ClusterIP}
		server.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
		server.Spec.IPFamilyPolicy = new(corev1.IPFamilyPolicySingleStack)
		server.Spec.InternalTrafficPolicy = new(corev1.ServiceInternalTrafficPolicyCluster)
		server.Spec.SessionAffinity = corev1.ServiceAffinityNone
		server.Spec.Ports[0].Protocol = corev1.ProtocolTCP
		if !matches(s, server) {
			t.Fatal("allocated service addresses considered drift")
		}
		server.Spec.Selector["unexpected"] = "label"
		if matches(s, server) {
			t.Fatal("additional service selector accepted")
		}
	}
}
