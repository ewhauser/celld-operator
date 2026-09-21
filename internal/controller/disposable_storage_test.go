package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// syncStorage simulates a conforming CSI provisioner, not the operator. It
// removes backend storage and releases the deletion finalizer after PVC absence.
func (x *operationFixture) syncStorage() {
	x.t.Helper()
	pvs := &corev1.PersistentVolumeList{}
	if err := x.r.List(x.t.Context(), pvs); err != nil {
		x.t.Fatal(err)
	}
	for _, pv := range pvs.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil {
			continue
		}
		c := &corev1.PersistentVolumeClaim{}
		if err := x.r.Get(x.t.Context(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, c); !apierrors.IsNotFound(err) {
			continue
		}
		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			continue
		}
		if err := x.r.Delete(x.t.Context(), &pv); err != nil {
			x.t.Fatal(err)
		}
		if err := x.r.Get(x.t.Context(), client.ObjectKeyFromObject(&pv), &pv); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			x.t.Fatal(err)
		}
		pv.Finalizers = nil
		if err := x.r.Update(x.t.Context(), &pv); err != nil {
			x.t.Fatal(err)
		}
	}
}
func storageCleanupFixture(t *testing.T) (*operationFixture, volumeIdentity) {
	t.Helper()
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	x.until("Observing")
	x.syncWorkload()
	x.until("DeleteClaims")
	return x, *x.state().Operation.Targets[0].Storage
}
func assertStorageProofHeld(t *testing.T, x *operationFixture) {
	t.Helper()
	o := x.state().Operation
	if o == nil || o.Phase != "DeleteClaims" || o.Targets[0].Proof == nil {
		t.Fatal("discarded cleanup authority or resumed compute")
	}
}
func TestDisposableStorageRequiresQualifiedFinalizer(t *testing.T) {
	for _, defect := range []string{"Retain", "no finalizer", "wrong provisioner", "wrong class", "unsupported source"} {
		t.Run(defect, func(t *testing.T) {
			x := newOperationFixture(t, "PersistentFleet")
			c := &corev1.PersistentVolumeClaim{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "data-alpha-2"}, c); err != nil {
				t.Fatal(err)
			}
			pv := &corev1.PersistentVolume{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Name: c.Spec.VolumeName}, pv); err != nil {
				t.Fatal(err)
			}
			switch defect {
			case "Retain":
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			case "no finalizer":
				pv.Finalizers = nil
			case "wrong provisioner":
				pv.Annotations["pv.kubernetes.io/provisioned-by"] = "another"
			case "wrong class":
				pv.Spec.StorageClassName = "another"
			case "unsupported source":
				pv.Spec.CSI = nil
				pv.Spec.HostPath = &corev1.HostPathVolumeSource{Path: "/data"}
			}
			if err := x.r.Update(t.Context(), pv); err != nil {
				t.Fatal(err)
			}
			x.desired(2)
			for range 3 {
				x.step()
			}
			if x.requests != 0 || x.state().Operation != nil || replicas(x.workload()) != 3 {
				t.Fatal("unqualified disk reached shutdown")
			}
		})
	}
}
func TestDisposableCleanupWaitsForCSIAndAttachments(t *testing.T) {
	x, v := storageCleanupFixture(t)
	x.step() // Persist cleanup intent before any delete.
	if !x.state().Operation.Targets[0].Storage.CleanupStarted {
		t.Fatal("cleanup intent not captured")
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: v.Claim}, claim); err != nil {
		t.Fatal("intent already deleted claim", err)
	}
	x.step() // Delete exact PVC, leave PV and CSI finalizer untouched.
	for range 3 {
		x.step()
		assertStorageProofHeld(t, x)
	}
	pv := &corev1.PersistentVolume{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Name: v.Volume}, pv); err != nil {
		t.Fatal(err)
	}
	if len(pv.Finalizers) != 1 {
		t.Fatal("operator altered CSI finalizer")
	}
	attachment := &storagev1.VolumeAttachment{Name: "pending-detach", Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: "host-2", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &v.Volume}}}
	if err := x.r.Create(t.Context(), attachment); err != nil {
		t.Fatal(err)
	}
	x.syncStorage() // CSI backend deletion finishes, attachment still observed.
	x.step()
	assertStorageProofHeld(t, x)
	if err := x.r.Delete(t.Context(), attachment); err != nil {
		t.Fatal(err)
	}
	x.finish()
	if x.state().Applied != 2 || x.state().Operation != nil {
		t.Fatal("cleanup did not complete")
	}
}
func TestDisposableCleanupRechecksEffectAndDrift(t *testing.T) {
	for _, drift := range []string{"workload", "policy", "handle", "pv uid", "pvc uid", "missing pvc", "missing pv", "foreign pod", "finalizer"} {
		t.Run(drift, func(t *testing.T) {
			x, v := storageCleanupFixture(t)
			c := &corev1.PersistentVolumeClaim{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: v.Claim}, c); err != nil {
				t.Fatal(err)
			}
			pv := &corev1.PersistentVolume{}
			if err := x.r.Get(t.Context(), client.ObjectKey{Name: v.Volume}, pv); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "workload":
				w := x.workload()
				setReplicas(w, 3)
				if err := x.r.Update(t.Context(), w); err != nil {
					t.Fatal(err)
				}
			case "policy":
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			case "handle":
				pv.Spec.CSI.VolumeHandle = "other-disk"
			case "pv uid":
				pv.UID = "replacement-volume"
			case "pvc uid":
				c.UID = "replacement-claim"
				if err := x.r.Update(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			case "missing pvc":
				if err := x.r.Delete(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			case "missing pv":
				pv.Finalizers = nil
				if err := x.r.Update(t.Context(), pv); err != nil {
					t.Fatal(err)
				}
				if err := x.r.Delete(t.Context(), pv); err != nil {
					t.Fatal(err)
				}
			case "foreign pod":
				p := &corev1.Pod{Name: "foreign", Namespace: x.f.Namespace, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.Claim}}}}}
				if err := x.r.Create(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "finalizer":
				pv.Finalizers = nil
			}
			if drift == "policy" || drift == "handle" || drift == "pv uid" || drift == "finalizer" {
				if err := x.r.Update(t.Context(), pv); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				x.step()
			}
			assertStorageProofHeld(t, x)
			if drift != "missing pvc" {
				if err := x.r.Get(t.Context(), client.ObjectKeyFromObject(c), c); err != nil {
					t.Fatal("unsafe claim delete", err)
				}
			}
		})
	}
}
func TestDisposableCleanupLostIntentAndDeleteResponses(t *testing.T) {
	for _, boundary := range []string{"intent", "delete"} {
		t.Run(boundary, func(t *testing.T) {
			x, _ := storageCleanupFixture(t)
			base := x.r.Client
			hit := false
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}
					if _, ok := obj.(*fleet.CelldStorageReservation); ok && boundary == "intent" && !hit {
						hit = true
						return errors.New("lost cleanup intent response")
					}
					return nil
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					if _, ok := obj.(*corev1.PersistentVolumeClaim); ok && boundary == "delete" && !hit {
						hit = true
						return errors.New("lost PVC delete response")
					}
					return nil
				},
			})
			x.finish()
			if !hit || x.state().Applied != 2 {
				t.Fatal("lost response not recovered")
			}
		})
	}
}
func TestDisposableCleanupStaleIssuerCannotDeleteReplacement(t *testing.T) {
	x, v := storageCleanupFixture(t)
	x.step()
	old := x.r.hydrate(t.Context(), envReservation(t, x.r, x.f))
	x.finish()
	x.desired(3)
	x.until("Intent")
	x.finish()
	if _, err := x.r.cleanupClaims(t.Context(), x.f, old); err == nil {
		t.Fatal("stale cleanup accepted")
	}
	replacement := &corev1.PersistentVolumeClaim{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: v.Claim}, replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.UID == v.ClaimUID {
		t.Fatal("growth reused old disk")
	}
	if _, err := x.r.cleanupClaims(t.Context(), x.f, x.r.hydrate(t.Context(), envReservation(t, x.r, x.f))); err == nil || !strings.Contains(err.Error(), "current operation") {
		t.Fatal("cleanup without authority", err)
	}
}

// Real API-server object identity, preconditions and protection finalizers;
// backend deletion is explicitly simulated because envtest has no CSI driver.
func TestEnvtestDisposableCleanup(t *testing.T) {
	r, e := envtestSetup(t, "PersistentFleet")
	e.provision(t, r)
	reconcile(t, r, e.fleet)
	ctx := t.Context()
	h := r.hydrate(ctx, envReservation(t, r, e.fleet))
	if h.err != nil || h.j == nil {
		t.Fatal("missing state", h.err)
	}
	w := emptyObject(workload(e.fleet, r.Options))
	if err := r.Get(ctx, client.ObjectKeyFromObject(e.fleet), w); err != nil {
		t.Fatal(err)
	}
	c := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: e.namespace, Name: "data-alpha-2"}, c); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{Name: "pvc-" + string(c.UID), Finalizers: []string{csiDeletionFinalizer}, Annotations: map[string]string{"pv.kubernetes.io/provisioned-by": "ebs.csi.aws.com"}, Spec: corev1.PersistentVolumeSpec{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: e.fleet.Spec.Storage.StorageClassName, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete, ClaimRef: &corev1.ObjectReference{Name: c.Name, Namespace: c.Namespace, UID: c.UID}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-" + string(c.UID)}}}}
	if err := r.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	c.Spec.VolumeName = pv.Name
	if err := r.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Status.Phase = corev1.ClaimBound
	if err := r.Status().Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	// The preceding strict proof path is covered separately. Build a valid exact
	// proof fixture, then exercise only the real Kubernetes cleanup boundary.
	x := newOperationFixture(t, "PersistentFleet")
	x.desired(2)
	x.until("ProofCaptured")
	target := x.state().Operation.Targets[0]
	v, err := r.targetVolume(ctx, e.fleet, h.j, target.Pod)
	if err != nil {
		t.Fatal(err)
	}
	target.Storage = v
	o := newOperation(e.fleet, h.j, w, time.Now().UTC(), "Scale", 2, "")
	o.Phase = "DeleteClaims"
	target.Proof.Removal.Operation = o.ID
	o.Targets = []operationTarget{target}
	h.j.Operation = o
	if err := r.saveState(ctx, h.res, h.j); err != nil {
		t.Fatal(err)
	}
	setReplicas(w, 2)
	annotations := w.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[operationKey] = effectID(o, false)
	w.SetAnnotations(annotations)
	if err := r.Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha-0", "alpha-1"} {
		p := &corev1.Pod{Name: name, Namespace: e.namespace, Labels: labels(e.fleet), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: e.fleet.Name, UID: w.GetUID(), Controller: new(true)}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "placeholder", Image: "placeholder:fixture"}}}}
		if err := r.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := func() (bool, error) {
		res := &fleet.CelldStorageReservation{}
		if err := r.Get(ctx, client.ObjectKey{Name: reservationName(e.fleet)}, res); err != nil {
			return false, err
		}
		h = r.hydrate(ctx, res)
		return r.cleanupClaims(ctx, e.fleet, h)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("intent", done, err)
	}
	stale := h
	// ResourceVersion is held in the operation. Editing the PVC after issuance
	// invalidates a delayed delete even when the name and UID stay the same.
	if err := r.Get(ctx, client.ObjectKeyFromObject(c), c); err != nil {
		t.Fatal(err)
	}
	c.Annotations["fixture.example/change"] = "rv"
	if err := r.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	old := stale.j.Operation.Targets[0].Storage
	if err := r.Delete(ctx, c, client.Preconditions{UID: &old.ClaimUID, ResourceVersion: &old.ClaimVersion}); !apierrors.IsConflict(err) {
		t.Fatal("stale PVC resourceVersion accepted", err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("rearm", done, err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("delete", done, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(c), c); err != nil || c.DeletionTimestamp.IsZero() {
		t.Fatal("PVC protection missing", err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("PVC finalizer bypassed", done, err)
	}
	c.Finalizers = nil
	if err := r.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("PV finalizer bypassed", done, err)
	}
	if err := r.Delete(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pv), pv); err != nil || pv.DeletionTimestamp.IsZero() {
		t.Fatal("PV deletion protection missing", err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("terminating PV treated as absent", done, err)
	}
	a := &storagev1.VolumeAttachment{Name: "detach-" + string(c.UID), Spec: storagev1.VolumeAttachmentSpec{Attacher: "ebs.csi.aws.com", NodeName: "host-2", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv.Name}}}
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Simulate CSI DeleteVolume completion, then the provisioner's finalizer release.
	pv.Finalizers = nil
	if err := r.Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if done, err := cleanup(); err != nil || done {
		t.Fatal("attachment ignored", done, err)
	}
	if err := r.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if done, err := cleanup(); err != nil || !done {
		t.Fatal("cleanup never completed", done, err)
	}
	if h.j.Operation.Targets[0].Proof == nil {
		t.Fatal("cleanup erased proof before lifecycle completion")
	}
}

func TestDisposableCleanupRechecksFinalizerAfterIntent(t *testing.T) {
	x, v := storageCleanupFixture(t)
	x.step() // Intent exists, but deletion has not been issued.
	pv := &corev1.PersistentVolume{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Name: v.Volume}, pv); err != nil {
		t.Fatal(err)
	}
	pv.Finalizers = nil
	if err := x.r.Update(t.Context(), pv); err != nil {
		t.Fatal(err)
	}
	x.step()
	assertStorageProofHeld(t, x)
	if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: v.Claim}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatal("deleted PVC after finalizer disappeared", err)
	}
}
func TestDisposableRestartWaitsAtZeroForAllDisks(t *testing.T) {
	x := newOperationFixture(t, "PersistentFleet")
	x.edit(func(f *fleet.CelldFleet) {
		f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "fresh-disks", AllowCoordinatedDowntime: true}
	})
	x.until("Observing")
	x.syncWorkload()
	x.until("DeleteClaims")
	old := x.state().Operation.Targets
	for range 5 {
		x.step()
	}
	if replicas(x.workload()) != 0 || x.state().Operation.Phase != "DeleteClaims" || len(x.state().Operation.Targets) != 3 {
		t.Fatal("restart escaped CSI wait")
	}
	pvs := &corev1.PersistentVolumeList{}
	if err := x.r.List(t.Context(), pvs); err != nil {
		t.Fatal(err)
	}
	if len(pvs.Items) != 3 {
		t.Fatal("operator deleted PVs")
	}
	x.finish()
	for _, target := range old {
		c := &corev1.PersistentVolumeClaim{}
		if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: target.Storage.Claim}, c); err != nil {
			t.Fatal(err)
		}
		if c.UID == target.Storage.ClaimUID || c.Spec.VolumeName == target.Storage.Volume {
			t.Fatal("restart reused completed disk")
		}
	}
}
func TestDisposableStateRejectsUnprotectedCleanup(t *testing.T) {
	x, _ := storageCleanupFixture(t)
	s := x.state()
	s.Operation.Targets[0].Storage.DeletionProtected = false
	if err := validateState(s); err == nil {
		t.Fatal("uncaptured finalizer accepted")
	}
	s.Operation.Targets[0].Storage.DeletionProtected = true
	s.Operation.Targets[0].Storage.CleanupStarted = true
	s.Operation.Phase = "ProofCaptured"
	if err := validateState(s); err == nil {
		t.Fatal("cleanup intent before compute removal accepted")
	}
}
