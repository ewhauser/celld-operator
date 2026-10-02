package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Admission can add init containers to a member. Waiting for one of those
// containers' images or configuration cannot be fixed by deleting its disk.
func TestReliabilityPersistentInitConfigurationRetainsDisk(t *testing.T) {
	for _, reason := range configWaits {
		t.Run(reason, func(t *testing.T) {
			x := agedFleet(t)
			x.admit = func(spec *corev1.PodSpec) {
				spec.InitContainers = append(spec.InitContainers, corev1.Container{Name: "admission-init", Image: "unavailable.example/init:latest"})
			}
			if err := x.r.Delete(t.Context(), x.pod("alpha-1")); err != nil {
				t.Fatal(err)
			}
			x.syncWorkload()
			before := x.claimUIDs()
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour), func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "admission-init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}
			})
			if action, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); err != nil || action != "" {
				t.Fatalf("configuration failure erased a member's disk: action=%q, err=%v", action, err)
			}
			if got := x.claimUIDs(); len(got) != len(before) || got["data-alpha-1"] != before["data-alpha-1"] {
				t.Fatalf("configuration failure changed disk identities: before=%v, after=%v", before, got)
			}
		})
	}
}

// A claim can recover after the observation that made it eligible for
// replacement. The same UID does not prove its Lost state is still current.
func TestReliabilityPersistentClaimRepairRacesDeletion(t *testing.T) {
	x := persistentFleet(t)
	c := x.claim("data-alpha-1")
	c.Status.Phase = corev1.ClaimLost
	if err := x.r.Status().Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	beforeClaims, beforePods := x.claimUIDs(), x.podUIDs()
	base := x.r.Client
	injected := false
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if claim, ok := obj.(*corev1.PersistentVolumeClaim); ok && claim.Name == "data-alpha-1" && !injected {
				injected = true
				current := &corev1.PersistentVolumeClaim{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(claim), current); err != nil {
					return err
				}
				current.Status.Phase = corev1.ClaimBound
				if err := c.Status().Update(ctx, current); err != nil {
					return err
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	_, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
	x.r.Client = base
	if !injected || !apierrors.IsConflict(err) {
		t.Fatalf("replacement ignored a concurrent claim repair: injected=%v, err=%v", injected, err)
	}
	if got := x.claimUIDs(); len(got) != len(beforeClaims) || got["data-alpha-1"] != beforeClaims["data-alpha-1"] {
		t.Fatalf("replacement deleted the repaired claim: before=%v, after=%v", beforeClaims, got)
	}
	if got := x.podUIDs(); len(got) != len(beforePods) || got["alpha-1"] != beforePods["alpha-1"] {
		t.Fatalf("replacement deleted the repaired claim's Pod: before=%v, after=%v", beforePods, got)
	}
	if action, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); err != nil || action != "" {
		t.Fatalf("retry failed to observe the repaired claim: action=%q, err=%v", action, err)
	}
}

// The StatefulSet can restart a down member between the Pod and claim lists.
// Its healthy successor still mounts the same retained claim. Deleting that
// claim before checking the Pod identity starts a replacement that the next
// reconcile then finishes by deleting the healthy successor too.
func TestReliabilityPersistentRecoveredPodRetainsDisk(t *testing.T) {
	for _, recovery := range []string{"same-pod-ready", "healthy-successor"} {
		t.Run(recovery, func(t *testing.T) {
			x := agedFleet(t)
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
			old := x.pod("alpha-1")
			claim := x.claim("data-alpha-1")
			claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
			if err := x.r.Update(t.Context(), claim); err != nil {
				t.Fatal(err)
			}
			before := x.claimUIDs()
			base := x.r.Client
			injected := false
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if _, claims := list.(*corev1.PersistentVolumeClaimList); !claims || injected {
						return nil
					}
					injected = true
					current := old.DeepCopy()
					current.Status.Conditions[0].Status = corev1.ConditionTrue
					if recovery == "healthy-successor" {
						if err := c.Delete(ctx, old); err != nil {
							return err
						}
						current.UID = "healthy-successor"
						current.ResourceVersion = ""
						return c.Create(ctx, current)
					}
					return c.Status().Update(ctx, current)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					// Model real UID preconditions, which the fake client omits.
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					if p, pod := obj.(*corev1.Pod); pod && options.Preconditions != nil && options.Preconditions.UID != nil {
						current := &corev1.Pod{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
							return err
						}
						if current.UID != *options.Preconditions.UID {
							return apierrors.NewConflict(corev1.Resource("pods"), p.Name, fmt.Errorf("Pod UID changed"))
						}
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			_, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
			x.r.Client = base
			if !x.claim("data-alpha-1").DeletionTimestamp.IsZero() {
				// Once claim deletion commits, the next reconcile treats it as
				// an interrupted replacement and deletes the healthy successor.
				_, _, _, retryErr := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true)
				_, podKept := x.podUIDs()["alpha-1"]
				t.Fatalf("stale observation committed disk deletion: first err=%v, retry err=%v, recovered Pod kept after retry=%v", err, retryErr, podKept)
			}
			if !injected || !apierrors.IsConflict(err) {
				t.Fatalf("stale down-member observation not rejected: injected=%v, err=%v", injected, err)
			}
			if got := x.claimUIDs(); len(got) != len(before) || got["data-alpha-1"] != before["data-alpha-1"] {
				t.Fatalf("recovered Pod lost its retained claim: before=%v, after=%v", before, got)
			}
			if p := x.pod("alpha-1"); !podReady(p) || recovery == "healthy-successor" && p.UID == old.UID {
				t.Fatalf("recovered Pod was not preserved: %+v", p)
			}
			x.operatorStep()
		})
	}
}

// Exercise both sides of an ambiguous Kubernetes response: the write may
// never have reached the API server, or it may have committed before timeout.
// A new reconciler must resume from the API state after either outcome.
func TestReliabilityPersistentReplacementDeleteFailuresAndRestart(t *testing.T) {
	for _, target := range []string{"claim", "pod"} {
		for _, committed := range []bool{false, true} {
			name := target + "/before-write"
			if committed {
				name = target + "/after-write"
			}
			t.Run(name, func(t *testing.T) {
				x := agedFleet(t)
				x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
				before := x.claimUIDs()
				c := x.claim("data-alpha-1")
				c.Finalizers = []string{"kubernetes.io/pvc-protection"}
				if err := x.r.Update(t.Context(), c); err != nil {
					t.Fatal(err)
				}
				base := x.r.Client
				injected := false
				x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						_, claim := obj.(*corev1.PersistentVolumeClaim)
						_, pod := obj.(*corev1.Pod)
						if obj.GetName() != "data-alpha-1" && obj.GetName() != "alpha-1" {
							t.Fatalf("replacement touched unrelated %T %s", obj, obj.GetName())
						}
						options := (&client.DeleteOptions{}).ApplyOptions(opts)
						if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != obj.GetUID() {
							t.Fatal("replacement delete did not pin the observed UID")
						}
						if !injected && (target == "claim" && claim || target == "pod" && pod) {
							injected = true
							if committed {
								if err := c.Delete(ctx, obj, opts...); err != nil {
									return err
								}
							}
							return apierrors.NewTimeoutError("injected ambiguous delete", 1)
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
				if _, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); !injected || !apierrors.IsTimeout(err) {
					t.Fatalf("fault did not fire: injected=%v, err=%v", injected, err)
				}
				// Restart discards all in-memory state; keep only Kubernetes objects.
				x.r = &Reconciler{Client: base, Options: x.r.Options, NetworkPolicyEnforced: true, now: func() time.Time { return x.clock }}
				if _, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); err != nil {
					t.Fatal(err)
				}
				if _, present := x.podUIDs()["alpha-1"]; present {
					t.Fatal("restart left the replaced Pod holding its terminating claim")
				}
				c = x.claim("data-alpha-1")
				if c.DeletionTimestamp.IsZero() {
					t.Fatal("restart did not resume deletion of the old claim")
				}
				// Simulate PVC protection after the last referencing Pod disappears.
				c.Finalizers = nil
				if err := x.r.Update(t.Context(), c); err != nil {
					t.Fatal(err)
				}
				x.clock = x.clock.Add(time.Second)
				x.converge()
				for name, uid := range x.claimUIDs() {
					if changed := uid != before[name]; changed != (name == "data-alpha-1") {
						t.Fatalf("restart changed the wrong disk %s: before=%s, after=%s", name, before[name], uid)
					}
				}
				x.storesNothing()
			})
		}
	}
}

// Missing observations and a second unhealthy member must never look like
// enough healthy survivors to discard the remaining down member's disk.
func TestReliabilityPersistentIncompleteSurvivorsRetainDisks(t *testing.T) {
	for _, fault := range []string{"missing-pod", "terminating-pod", "readiness-unknown", "recently-ready", "ready-without-transition"} {
		t.Run(fault, func(t *testing.T) {
			x := agedFleet(t)
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
			before := x.claimUIDs()
			p := x.pod("alpha-2")
			switch fault {
			case "missing-pod":
				if err := x.r.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "terminating-pod":
				p.Finalizers = []string{"test.celld.eric.dev/hold"}
				if err := x.r.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
				if err := x.r.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			case "readiness-unknown":
				x.setMember("alpha-2", x.clock.Add(-2*time.Hour), corev1.ConditionUnknown, x.clock.Add(-time.Hour))
			case "recently-ready":
				x.setMember("alpha-2", x.clock.Add(-2*time.Hour), corev1.ConditionTrue, x.clock.Add(-absorbWindow+time.Second))
			case "ready-without-transition":
				x.setMember("alpha-2", x.clock.Add(-2*time.Hour), corev1.ConditionTrue, time.Time{})
			}
			x.operatorStep()
			if got := x.claimUIDs(); len(got) != len(before) || got["data-alpha-1"] != before["data-alpha-1"] {
				t.Fatalf("incomplete survivors released a disk: before=%v, after=%v", before, got)
			}
		})
	}
}

// A real API server enforces both delete preconditions. The fake client does
// not enforce UID, so name reuse must be qualified here as well as tested with
// intercepted failure responses above.
func TestEnvtestReliabilityPersistentReplacementClaimChanged(t *testing.T) {
	for _, change := range []string{"name-reused", "phase-repaired", "ownership-changed"} {
		t.Run(change, func(t *testing.T) {
			r, x := envtestSetup(t, "PersistentFleet")
			x.provision(t, r)
			tmpl := workload(x.fleet, r.Options).(*appsv1.StatefulSet).Spec.VolumeClaimTemplates[0]
			old := &corev1.PersistentVolumeClaim{Name: claimName(x.fleet, 1), Namespace: x.fleet.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations, Spec: tmpl.Spec}
			if err := r.Create(t.Context(), old); err != nil {
				t.Fatal(err)
			}
			old.Status.Phase = corev1.ClaimLost
			if err := r.Status().Update(t.Context(), old); err != nil {
				t.Fatal(err)
			}
			observed := old.DeepCopy()
			current := old.DeepCopy()
			switch change {
			case "name-reused":
				if err := r.Delete(t.Context(), current); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(old), current); err != nil {
					t.Fatal(err)
				}
				// There are no Pods here; simulate the absent protection controller.
				current.Finalizers = nil
				if err := r.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
				current = &corev1.PersistentVolumeClaim{Name: old.Name, Namespace: old.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations, Spec: tmpl.Spec}
				if err := r.Create(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			case "phase-repaired":
				current.Status.Phase = corev1.ClaimBound
				if err := r.Status().Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			case "ownership-changed":
				current.Labels[FleetLabel] = "another-fleet"
				if err := r.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.replaceMember(t.Context(), observed, nil); !apierrors.IsConflict(err) {
				t.Fatalf("stale replacement deleted a changed claim: %v", err)
			}
			got := &corev1.PersistentVolumeClaim{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(current), got); err != nil {
				t.Fatal(err)
			}
			if got.UID != current.UID || !got.DeletionTimestamp.IsZero() || got.Labels[FleetLabel] != current.Labels[FleetLabel] || got.Status.Phase != current.Status.Phase {
				t.Fatalf("stale replacement mutated the changed claim: current=%+v, got=%+v", current, got)
			}
		})
	}
}

// API read failures provide no evidence for releasing a disk; both member
// observations and claim observations are required before any deletion.
func TestReliabilityPersistentObservationFailuresRetainDisks(t *testing.T) {
	for _, kind := range []string{"pods", "claims", "victim-read"} {
		t.Run(kind, func(t *testing.T) {
			x := agedFleet(t)
			x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
			base := x.r.Client
			injected := false
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, pod := obj.(*corev1.Pod); kind == "victim-read" && pod && key.Name == "alpha-1" {
						injected = true
						return apierrors.NewTimeoutError("injected unavailable victim", 1)
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					_, pods := list.(*corev1.PodList)
					_, claims := list.(*corev1.PersistentVolumeClaimList)
					if kind == "pods" && pods || kind == "claims" && claims {
						injected = true
						return apierrors.NewTimeoutError("injected unavailable observation", 1)
					}
					return c.List(ctx, list, opts...)
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					t.Fatal("operator deleted storage or a Pod without complete observations")
					return nil
				},
			})
			if _, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); !injected || !apierrors.IsTimeout(err) {
				t.Fatalf("observation failure ignored: injected=%v, err=%v", injected, err)
			}
			x.r.Client = base
		})
	}
}

// Cleanup observations can go stale just like replacement observations. A
// claim moved to another fleet after List must survive the old fleet's cleanup.
func TestReliabilityPersistentCleanupClaimOwnershipChanged(t *testing.T) {
	x := persistentFleet(t)
	base := x.r.Client
	injected := false
	x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			claim := obj.(*corev1.PersistentVolumeClaim)
			current := &corev1.PersistentVolumeClaim{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(claim), current); err != nil {
				return err
			}
			current.Labels[FleetLabel] = "another-fleet"
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			injected = true
			return c.Delete(ctx, obj, opts...)
		},
	})
	_, err := x.r.deleteClaims(t.Context(), x.f)
	x.r.Client = base
	if !injected || !apierrors.IsConflict(err) {
		t.Fatalf("cleanup ignored same-UID ownership drift: injected=%v, err=%v", injected, err)
	}
	list := &corev1.PersistentVolumeClaimList{}
	if err := x.r.List(t.Context(), list, client.InNamespace(x.f.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("cleanup deleted a changed claim: kept=%d", len(list.Items))
	}
}

func TestEnvtestReliabilityPersistentCleanupClaimOwnershipChanged(t *testing.T) {
	r, x := envtestSetup(t, "PersistentFleet")
	x.provision(t, r)
	tmpl := workload(x.fleet, r.Options).(*appsv1.StatefulSet).Spec.VolumeClaimTemplates[0]
	claim := &corev1.PersistentVolumeClaim{Name: claimName(x.fleet, 1), Namespace: x.fleet.Namespace, Labels: tmpl.Labels, Annotations: tmpl.Annotations, Spec: tmpl.Spec}
	if err := r.Create(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	injected := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, claims := list.(*corev1.PersistentVolumeClaimList); claims && !injected {
				current := claim.DeepCopy()
				current.Labels[FleetLabel] = "another-fleet"
				if err := c.Update(ctx, current); err != nil {
					return err
				}
				injected = true
			}
			return nil
		},
	})
	_, err := r.deleteClaims(t.Context(), x.fleet)
	r.Client = base
	if !injected || !apierrors.IsConflict(err) {
		t.Fatalf("cleanup ignored same-UID ownership drift: injected=%v, err=%v", injected, err)
	}
	current := &corev1.PersistentVolumeClaim{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), current); err != nil {
		t.Fatal(err)
	}
	if current.UID != claim.UID || !current.DeletionTimestamp.IsZero() || current.Labels[FleetLabel] != "another-fleet" {
		t.Fatalf("cleanup mutated a changed claim: %+v", current)
	}
}
