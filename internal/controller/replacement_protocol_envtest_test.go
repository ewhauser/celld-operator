package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The API server assigns creation timestamps, so these tests isolate an
// already-selected replacement. Fake-controller tests cover eligibility;
// these cover actual delete CAS, finalizer retention and restart authority.
func replacementEnvtestFixture(t *testing.T) (*Reconciler, *envtestFixture, *corev1.Pod, *corev1.PersistentVolumeClaim) {
	t.Helper()
	r, x := envtestSetup(t, "PersistentFleet")
	x.provision(t, r)
	sts := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), sts); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var victim *corev1.Pod
	var claim *corev1.PersistentVolumeClaim
	for ordinal := range replicas(sts) {
		template := sts.Spec.VolumeClaimTemplates[0]
		pvc := &corev1.PersistentVolumeClaim{
			Name: claimName(x.fleet, ordinal), Namespace: x.namespace,
			Labels: labels(x.fleet), Annotations: template.Annotations,
			Finalizers: []string{"kubernetes.io/pvc-protection"}, Spec: template.Spec,
		}
		if err := r.Create(t.Context(), pvc); err != nil {
			t.Fatal(err)
		}
		pvc.Status.Phase = corev1.ClaimBound
		if err := r.Status().Update(t.Context(), pvc); err != nil {
			t.Fatal(err)
		}
		spec := sts.Spec.Template.Spec.DeepCopy()
		spec.NodeName = fmt.Sprintf("replacement-node-%d", ordinal)
		// There is no kubelet in envtest. Zero grace lets the server remove
		// the object as soon as its finalizers have been released.
		spec.TerminationGracePeriodSeconds = new(int64(0))
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "data", PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}})
		p := &corev1.Pod{
			Name: memberName(x.fleet, ordinal), Namespace: x.namespace, Labels: labels(x.fleet),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: new(true)}},
			Spec:            *spec,
		}
		if err := r.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		ready := corev1.ConditionTrue
		if ordinal == 1 {
			ready = corev1.ConditionFalse
		}
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}}}
		if err := r.Status().Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if ordinal == 1 {
			victim, claim = p.DeepCopy(), pvc.DeepCopy()
		}
	}
	if victim == nil || claim == nil {
		t.Fatal("replacement fixture requires member ordinal 1")
	}
	return r, x, victim, claim
}

func replacementEnvtestRestart(t *testing.T, r *Reconciler, x *envtestFixture) {
	t.Helper()
	restarted := &Reconciler{Client: x.client, Options: r.Options, NetworkPolicyEnforced: true}
	if _, err := restarted.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.fleet)}); err != nil {
		t.Fatal(err)
	}
}

// Recovery in the last window before the Pod delete must reject that delete
// while the disk is still intact. The prepared hold must also disappear on
// restart; it is not authority to retire the recovered member later.
func TestEnvtestReliabilityReplacementRecoveryBeforePodDelete(t *testing.T) {
	r, x, victim, claim := replacementEnvtestFixture(t)
	base := r.Client
	injected := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if p, pod := obj.(*corev1.Pod); pod && p.UID == victim.UID && !injected {
			current := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(victim), current); err != nil {
				return err
			}
			current.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
			if err := c.Status().Update(ctx, current); err != nil {
				return err
			}
			injected = true
		}
		return c.Delete(ctx, obj, opts...)
	}})
	err := r.replaceMember(t.Context(), claim, victim)
	if !injected || !apierrors.IsConflict(err) {
		t.Fatalf("guarded Pod delete accepted recovery: injected=%t err=%v", injected, err)
	}
	r.Client = base
	replacementEnvtestRestart(t, r, x)
	current := &corev1.Pod{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(victim), current); err != nil {
		t.Fatal(err)
	}
	kept := &corev1.PersistentVolumeClaim{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil {
		t.Fatal(err)
	}
	if current.UID != victim.UID || !current.DeletionTimestamp.IsZero() || !podReady(current) || slices.Contains(current.Finalizers, replacementFinalizer) || current.Annotations[replacementAnnotation] != "" {
		t.Fatalf("restart did not preserve recovered Pod and abort its hold: %+v", current)
	}
	if kept.UID != claim.UID || !kept.DeletionTimestamp.IsZero() {
		t.Fatal("recovery before Pod delete erased the retained disk")
	}
}

func replacementEnvtestStage(obj client.Object) string {
	switch obj := obj.(type) {
	case *corev1.PersistentVolumeClaim:
		return "pvc-delete"
	case *corev1.Pod:
		if !slices.Contains(obj.Finalizers, replacementFinalizer) {
			return "release"
		}
		if obj.DeletionTimestamp.IsZero() {
			return "prepare"
		}
		intent, ok := replacementIntentOf(obj)
		if ok && intent.Committed {
			return "commit"
		}
		return "pod-delete"
	}
	return ""
}

// Interrupt every mutating boundary both before the API write and after its
// successful response is lost. A fresh reconciler must either finish a durable
// commitment or drop an uncertain preparation while keeping the disk.
func TestEnvtestReliabilityReplacementProtocolFaultReplay(t *testing.T) {
	for _, stage := range []string{"prepare", "pod-delete", "commit", "pvc-delete", "release"} {
		for _, after := range []bool{false, true} {
			suffix := "before-write"
			if after {
				suffix = "lost-response"
			}
			t.Run(stage+"/"+suffix, func(t *testing.T) {
				r, x, victim, claim := replacementEnvtestFixture(t)
				base := r.Client
				injected := false
				fault := func(obj client.Object, deleting bool, invoke func() error) error {
					actual := replacementEnvtestStage(obj)
					if p, pod := obj.(*corev1.Pod); pod && deleting && p.UID == victim.UID {
						actual = "pod-delete"
					}
					if actual != stage || injected {
						return invoke()
					}
					injected = true
					if after {
						if err := invoke(); err != nil {
							return err
						}
					}
					return apierrors.NewTimeoutError("injected replacement API response loss", 1)
				}
				r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						return fault(obj, false, func() error { return c.Update(ctx, obj, opts...) })
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						return fault(obj, true, func() error { return c.Delete(ctx, obj, opts...) })
					},
				})
				err := r.replaceMember(t.Context(), claim, victim)
				if !injected || !apierrors.IsTimeout(err) {
					t.Fatalf("replacement boundary did not inject timeout: injected=%t err=%v", injected, err)
				}
				r.Client = base
				replacementEnvtestRestart(t, r, x)
				kept := &corev1.PersistentVolumeClaim{}
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil {
					t.Fatal(err)
				}
				expectDeletion := stage == "pvc-delete" || stage == "release" || stage == "commit" && after
				if kept.UID != claim.UID || !kept.DeletionTimestamp.IsZero() != expectDeletion {
					t.Fatalf("replay changed disk without durable commitment or failed to finish: stage=%s after=%t deletionTimestamp=%s", stage, after, kept.DeletionTimestamp)
				}
				current := &corev1.Pod{}
				err = base.Get(t.Context(), client.ObjectKeyFromObject(victim), current)
				if err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
				if err == nil && (current.UID != victim.UID || slices.Contains(current.Finalizers, replacementFinalizer) || current.Annotations[replacementAnnotation] != "") {
					t.Fatal("restart leaked its replacement hold or changed Pod identity")
				}
				if stage == "prepare" || stage == "pod-delete" && !after {
					if err != nil || !current.DeletionTimestamp.IsZero() {
						t.Fatalf("uncommitted preparation deleted the member: %v", err)
					}
				}
			})
		}
	}
}

func replacementEnvtestHoldCommitted(t *testing.T, r *Reconciler, victim *corev1.Pod, claim *corev1.PersistentVolumeClaim) {
	t.Helper()
	base := r.Client
	injected := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.UID == claim.UID {
			injected = true
			return apierrors.NewTimeoutError("interrupted after Pod deletion commitment", 1)
		}
		return c.Delete(ctx, obj, opts...)
	}})
	err := r.replaceMember(t.Context(), claim, victim)
	r.Client = base
	if !injected || !apierrors.IsTimeout(err) {
		t.Fatalf("failed to hold a committed replacement: injected=%t err=%v", injected, err)
	}
}

func TestEnvtestReliabilityReplacementClaimRepairAfterCommit(t *testing.T) {
	r, x, victim, claim := replacementEnvtestFixture(t)
	claim.Status.Phase = corev1.ClaimLost
	if err := r.Status().Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	replacementEnvtestHoldCommitted(t, r, victim, claim)
	current := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), current); err != nil {
		t.Fatal(err)
	}
	current.Status.Phase = corev1.ClaimBound
	if err := r.Status().Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	replacementEnvtestRestart(t, r, x)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), current); err != nil {
		t.Fatal(err)
	}
	if current.UID != claim.UID || current.Status.Phase != corev1.ClaimBound || !current.DeletionTimestamp.IsZero() {
		t.Fatal("restart deleted a claim repaired after the original replacement observation")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("abort leaked the deleted Pod's name hold: %v", err)
	}
}

func TestEnvtestReliabilityReplacementExternalDeleteBeforeCommit(t *testing.T) {
	r, x, victim, claim := replacementEnvtestFixture(t)
	base := r.Client
	injected := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if err := c.Update(ctx, obj, opts...); err != nil {
			return err
		}
		if p, pod := obj.(*corev1.Pod); pod && p.UID == victim.UID && !injected {
			current := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(victim), current); err != nil {
				return err
			}
			current.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
			if err := c.Status().Update(ctx, current); err != nil {
				return err
			}
			// A rollout, eviction or another controller may delete the prepared
			// Pod without using the operator's observed resourceVersion.
			if err := c.Delete(ctx, current); err != nil {
				return err
			}
			injected = true
			return apierrors.NewTimeoutError("process lost after preparation and external deletion", 1)
		}
		return nil
	}})
	err := r.replaceMember(t.Context(), claim, victim)
	if !injected || !apierrors.IsTimeout(err) {
		t.Fatalf("external deletion window was not injected: injected=%t err=%v", injected, err)
	}
	r.Client = base
	replacementEnvtestRestart(t, r, x)
	current := &corev1.PersistentVolumeClaim{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(claim), current); err != nil {
		t.Fatal(err)
	}
	if current.UID != claim.UID || !current.DeletionTimestamp.IsZero() {
		t.Fatal("external Pod deletion masqueraded as guarded replacement commitment")
	}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("abort leaked the external deletion's name hold: %v", err)
	}
}

// The discovery identity for our hold must survive changes to the normal
// workload label or owner. Drift revokes disk authority but cannot leave our
// own finalizer permanently blocking a Pod now managed by another controller.
func TestEnvtestReliabilityReplacementOwnershipDriftAbortsHold(t *testing.T) {
	for _, drift := range []string{"owner-reference", "fleet-label"} {
		t.Run(drift, func(t *testing.T) {
			r, x, victim, claim := replacementEnvtestFixture(t)
			replacementEnvtestHoldCommitted(t, r, victim, claim)
			current := &corev1.Pod{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), current); err != nil {
				t.Fatal(err)
			}
			if drift == "owner-reference" {
				current.OwnerReferences[0].Name = "another-workload"
				current.OwnerReferences[0].UID = "another-workload-uid"
			} else {
				current.Labels[FleetLabel] = "another-fleet-uid"
			}
			if err := r.Update(t.Context(), current); err != nil {
				t.Fatal(err)
			}
			replacementEnvtestRestart(t, r, x)
			kept := &corev1.PersistentVolumeClaim{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil {
				t.Fatal(err)
			}
			if kept.UID != claim.UID || !kept.DeletionTimestamp.IsZero() {
				t.Fatal("ownership drift retained authority to delete the previous fleet's disk")
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("ownership drift leaked the terminating Pod's own hold: %v", err)
			}
		})
	}
}

// Authority must also be checked in the uninterrupted path. A foreign owner
// can adopt the held Pod after its deletion was accepted but before the
// operator records a durable commitment to delete the claim.
func TestEnvtestReliabilityReplacementAuthorityChangesAfterPodDelete(t *testing.T) {
	for _, drift := range []string{"owner-reference", "fleet-label"} {
		t.Run(drift, func(t *testing.T) {
			r, x, victim, claim := replacementEnvtestFixture(t)
			base := r.Client
			injected := false
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				if p, pod := obj.(*corev1.Pod); pod && p.UID == victim.UID && !injected {
					current := &corev1.Pod{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(victim), current); err != nil {
						return err
					}
					if drift == "owner-reference" {
						current.OwnerReferences[0].Name = "another-workload"
						current.OwnerReferences[0].UID = "another-workload-uid"
					} else {
						current.Labels[FleetLabel] = "another-fleet-uid"
					}
					if err := c.Update(ctx, current); err != nil {
						return err
					}
					injected = true
				}
				return nil
			}})
			if err := r.replaceMember(t.Context(), claim, victim); !apierrors.IsConflict(err) || !injected {
				t.Fatalf("authority change was not safely aborted: injected=%t err=%v", injected, err)
			}
			r.Client = base
			replacementEnvtestRestart(t, r, x)
			kept := &corev1.PersistentVolumeClaim{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil {
				t.Fatal(err)
			}
			if kept.UID != claim.UID || !kept.DeletionTimestamp.IsZero() {
				t.Fatal("changed Pod authority authorized disk deletion in the immediate path")
			}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("authority change leaked the terminating Pod's own hold: %v", err)
			}
		})
	}
}

func TestEnvtestReliabilityReplacementDeletingWorkloadAbortsHold(t *testing.T) {
	r, x, victim, claim := replacementEnvtestFixture(t)
	replacementEnvtestHoldCommitted(t, r, victim, claim)
	sts := &appsv1.StatefulSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), sts); err != nil {
		t.Fatal(err)
	}
	// Retain the terminating workload so resumption must recognize its
	// deletion state, rather than relying on a NotFound response.
	sts.Finalizers = []string{"another.example.com/workload-hold"}
	if err := r.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	replacementEnvtestRestart(t, r, x)
	kept := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil {
		t.Fatal(err)
	}
	if kept.UID != claim.UID || !kept.DeletionTimestamp.IsZero() {
		t.Fatal("terminating workload authorized a new disk deletion")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workload deletion leaked the terminating Pod's own hold: %v", err)
	}
}

func TestEnvtestReliabilityReplacementHeldNameAndForeignFinalizer(t *testing.T) {
	r, x, victim, claim := replacementEnvtestFixture(t)
	const foreign = "another.example.com/hold"
	victim.Finalizers = append(victim.Finalizers, foreign)
	victim.Annotations = map[string]string{"another.example.com/annotation": "keep"}
	if err := r.Update(t.Context(), victim); err != nil {
		t.Fatal(err)
	}
	replacementEnvtestHoldCommitted(t, r, victim, claim)
	current := &corev1.Pod{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), current); err != nil {
		t.Fatal(err)
	}
	if current.DeletionTimestamp.IsZero() || !slices.Contains(current.Finalizers, replacementFinalizer) {
		t.Fatal("committed replacement did not retain the terminating Pod name")
	}
	successor := &corev1.Pod{Name: victim.Name, Namespace: victim.Namespace, Labels: labels(x.fleet), OwnerReferences: victim.OwnerReferences, Spec: victim.Spec}
	if err := r.Create(t.Context(), successor); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("real API permitted name reuse before claim deletion: %v", err)
	}
	replacementEnvtestRestart(t, r, x)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(victim), current); err != nil {
		t.Fatal(err)
	}
	if current.UID != victim.UID || slices.Contains(current.Finalizers, replacementFinalizer) || !slices.Contains(current.Finalizers, foreign) || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" || current.Annotations["another.example.com/annotation"] != "keep" {
		t.Fatal("replacement release erased another controller's metadata or retained its own hold")
	}
	kept := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), kept); err != nil || kept.DeletionTimestamp.IsZero() {
		t.Fatalf("replacement released its name before the disk deletion was accepted: %v", err)
	}
	// Simulate only the unrelated controller's completion. Its finalizer
	// remained intact until it removed that key itself.
	current.Finalizers = slices.DeleteFunc(current.Finalizers, func(s string) bool { return s == foreign })
	if err := r.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(t.Context(), successor); err != nil {
		t.Fatalf("name remained blocked after every finalizer completed: %v", err)
	}
	if successor.UID == victim.UID {
		t.Fatal("real API reused the previous Pod UID")
	}
}
