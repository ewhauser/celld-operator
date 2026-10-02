package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func replacementProtocolFixture(t *testing.T) (*operationFixture, *corev1.Pod, *corev1.PersistentVolumeClaim) {
	t.Helper()
	x := agedFleet(t)
	x.setMember("alpha-1", x.clock.Add(-2*time.Hour), corev1.ConditionFalse, x.clock.Add(-time.Hour))
	c := x.claim("data-alpha-1")
	c.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if err := x.r.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	p := x.pod("alpha-1")
	c = x.claim("data-alpha-1")
	return x, p, c
}

func recordReplacementIntent(t *testing.T, x *operationFixture, p *corev1.Pod, c *corev1.PersistentVolumeClaim, committed bool) *corev1.Pod {
	t.Helper()
	p = p.DeepCopy()
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	setReplacementIntent(p, replacementIntent{ClaimUID: c.UID, ClaimResourceVersion: c.ResourceVersion, Committed: committed})
	p.Finalizers = append(p.Finalizers, replacementFinalizer)
	if err := x.r.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if committed {
		if err := x.r.Delete(t.Context(), p, client.Preconditions{UID: new(p.UID), ResourceVersion: new(p.ResourceVersion)}); err != nil {
			t.Fatal(err)
		}
	}
	return x.pod(p.Name)
}

func restartReplacementReconciler(x *operationFixture, base client.Client) {
	x.r = &Reconciler{Client: base, Options: x.r.Options, NetworkPolicyEnforced: true, now: func() time.Time { return x.clock }}
}

func reconcileReplacementOnce(t *testing.T, x *operationFixture) {
	t.Helper()
	if _, err := x.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.f)}); err != nil {
		t.Fatal(err)
	}
}

// Losing an acknowledgement is different from losing the write. Exercise
// both outcomes at every persistent transition, then discard the reconciler
// and require restart to choose the safe outcome from Kubernetes state alone.
func TestReliabilityPersistentReplacementProtocolAmbiguousWrites(t *testing.T) {
	for _, stage := range []string{"prepare", "delete-pod", "commit", "delete-claim", "release"} {
		for _, persisted := range []bool{false, true} {
			name := stage + "/before-write"
			if persisted {
				name = stage + "/after-write"
			}
			t.Run(name, func(t *testing.T) {
				x, p, claim := replacementProtocolFixture(t)
				beforeClaims := x.claimUIDs()
				base := x.r.Client
				injected := false
				updateStage := func(obj client.Object) string {
					p, pod := obj.(*corev1.Pod)
					if !pod || p.Name != "alpha-1" {
						return ""
					}
					if !slices.Contains(p.Finalizers, replacementFinalizer) {
						return "release"
					}
					var intent replacementIntent
					if err := json.Unmarshal([]byte(p.Annotations[replacementAnnotation]), &intent); err != nil {
						t.Fatal(err)
					}
					if intent.FleetUID != x.f.UID || p.Labels[replacementFleetLabel] != string(x.f.UID) || intent.ClaimUID != claim.UID || intent.ClaimResourceVersion != claim.ResourceVersion {
						t.Fatal("intent did not preserve the originally observed claim UID/version")
					}
					if intent.Committed {
						return "commit"
					}
					return "prepare"
				}
				x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if !injected && updateStage(obj) == stage {
							injected = true
							if persisted {
								if err := c.Update(ctx, obj, opts...); err != nil {
									return err
								}
							}
							return apierrors.NewTimeoutError("injected lost "+stage+" acknowledgement", 1)
						}
						return c.Update(ctx, obj, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						deleteStage := "delete-pod"
						if _, pvc := obj.(*corev1.PersistentVolumeClaim); pvc {
							deleteStage = "delete-claim"
							currentPod := &corev1.Pod{}
							if err := c.Get(ctx, client.ObjectKeyFromObject(p), currentPod); err != nil {
								return err
							}
							var intent replacementIntent
							if err := json.Unmarshal([]byte(currentPod.Annotations[replacementAnnotation]), &intent); err != nil {
								t.Fatal(err)
							}
							if currentPod.DeletionTimestamp.IsZero() || !intent.Committed || !slices.Contains(currentPod.Finalizers, replacementFinalizer) {
								t.Fatal("claim deletion preceded an acknowledged, durably committed held Pod deletion")
							}
						}
						options := (&client.DeleteOptions{}).ApplyOptions(opts)
						if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
							t.Fatal("replacement delete did not guard both UID and resourceVersion")
						}
						if !injected && deleteStage == stage {
							injected = true
							if persisted {
								if err := c.Delete(ctx, obj, opts...); err != nil {
									return err
								}
							}
							return apierrors.NewTimeoutError("injected lost "+stage+" acknowledgement", 1)
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
				if err := x.r.replaceMember(t.Context(), claim, p); !injected || !apierrors.IsTimeout(err) {
					t.Fatalf("fault did not fire: injected=%v, err=%v", injected, err)
				}
				restartReplacementReconciler(x, base)
				// With no recorded hold, no restart work is needed. Otherwise the
				// early reconcile hook must resolve it before ordinary healing.
				if _, present := x.podUIDs()["alpha-1"]; present && slices.Contains(x.pod("alpha-1").Finalizers, replacementFinalizer) {
					reconcileReplacementOnce(t, x)
				}
				currentClaim := x.claim("data-alpha-1")
				wantFresh := stage == "commit" && persisted || stage == "delete-claim" || stage == "release"
				if deleting := !currentClaim.DeletionTimestamp.IsZero(); deleting != wantFresh {
					t.Fatalf("restart chose wrong disk outcome: terminating=%v, want replacement=%v", deleting, wantFresh)
				}
				if currentClaim.UID != claim.UID {
					t.Fatal("restart targeted a different claim identity")
				}
				if p, present := x.podUIDs()["alpha-1"]; present {
					current := x.pod("alpha-1")
					if current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" || slices.Contains(current.Finalizers, replacementFinalizer) {
						t.Fatal("restart left a replacement hold behind")
					}
					if wantFresh {
						t.Fatalf("old Pod %s still holds the replaced claim", p)
					}
					// Model recovery after abort so ordinary healing does not
					// select a new, unrelated replacement in the convergence loop.
					x.setMember("alpha-1", current.CreationTimestamp.Time, corev1.ConditionTrue, x.clock)
				}
				if wantFresh {
					// No Pod references the terminating claim; simulate protection.
					currentClaim.Finalizers = nil
					if err := x.r.Update(t.Context(), currentClaim); err != nil {
						t.Fatal(err)
					}
				}
				x.clock = x.clock.Add(time.Second)
				x.converge()
				for name, uid := range x.claimUIDs() {
					if changed := uid != beforeClaims[name]; changed != (name == "data-alpha-1" && wantFresh) {
						t.Fatalf("restart changed wrong disk %s: changed=%v, want replacement=%v", name, changed, wantFresh)
					}
				}
			})
		}
	}
}

func TestReliabilityPersistentPreparedReplacementRecoveryAndExternalDeletion(t *testing.T) {
	for _, externalDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("externalDelete=%v", externalDelete), func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			recordReplacementIntent(t, x, p, c, false)
			x.setMember("alpha-1", p.CreationTimestamp.Time, corev1.ConditionTrue, x.clock)
			if externalDelete {
				if err := x.r.Delete(t.Context(), x.pod("alpha-1")); err != nil {
					t.Fatal(err)
				}
			}
			restartReplacementReconciler(x, x.r.Client)
			reconcileReplacementOnce(t, x)
			if claim := x.claim(c.Name); claim.UID != c.UID || !claim.DeletionTimestamp.IsZero() {
				t.Fatal("an uncommitted or externally deleted prepared Pod authorized claim deletion")
			}
			if !externalDelete {
				current := x.pod(p.Name)
				if current.UID != p.UID || !podReady(current) || slices.Contains(current.Finalizers, replacementFinalizer) || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" {
					t.Fatal("restart did not preserve the recovered live member and clear its hold")
				}
			} else if _, present := x.podUIDs()[p.Name]; present {
				t.Fatal("uncommitted pin blocked an externally requested Pod deletion")
			}
		})
	}
}

func TestReliabilityPersistentReplacementPreservesForeignMetadata(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			p.Finalizers = []string{"other.example/retain"}
			p.Annotations = map[string]string{"other.example/note": "keep-me"}
			p.Labels["other.example/label"] = "keep-me"
			if err := x.r.Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			recordReplacementIntent(t, x, x.pod(p.Name), c, committed)
			if handled, err := x.r.resumeReplacements(t.Context(), x.f); err != nil || !handled {
				t.Fatalf("replacement did not resume: handled=%v, err=%v", handled, err)
			}
			current := x.pod(p.Name)
			if !slices.Equal(current.Finalizers, []string{"other.example/retain"}) || current.Annotations["other.example/note"] != "keep-me" || current.Labels["other.example/label"] != "keep-me" || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" {
				t.Fatalf("replacement cleanup damaged foreign metadata: %+v", current.ObjectMeta)
			}
			if deleting := !x.claim(c.Name).DeletionTimestamp.IsZero(); deleting != committed {
				t.Fatalf("wrong claim result: terminating=%v, committed=%v", deleting, committed)
			}
		})
	}
}

func TestReliabilityPersistentCommittedReplacementRetainsChangedClaim(t *testing.T) {
	for _, change := range []string{"phase-repaired", "ownership-changed", "name-reused"} {
		t.Run(change, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			if change == "phase-repaired" {
				c.Status.Phase = corev1.ClaimLost
				if err := x.r.Status().Update(t.Context(), c); err != nil {
					t.Fatal(err)
				}
				c = x.claim(c.Name)
			}
			recordReplacementIntent(t, x, p, c, true)
			current := x.claim(c.Name)
			switch change {
			case "phase-repaired":
				current.Status.Phase = corev1.ClaimBound
				if err := x.r.Status().Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			case "ownership-changed":
				current.Labels[FleetLabel] = "another-fleet"
				if err := x.r.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			case "name-reused":
				current.Finalizers = nil
				if err := x.r.Update(t.Context(), current); err != nil {
					t.Fatal(err)
				}
				if err := x.r.Delete(t.Context(), current); err != nil {
					t.Fatal(err)
				}
				current.ResourceVersion, current.UID = "", types.UID("replacement-claim")
				current.CreationTimestamp = metav1.Time{}
				if err := x.r.Create(t.Context(), current); err != nil {
					t.Fatal(err)
				}
			}
			restartReplacementReconciler(x, x.r.Client)
			reconcileReplacementOnce(t, x)
			got := x.claim(c.Name)
			if got.UID != current.UID || !got.DeletionTimestamp.IsZero() || got.Labels[FleetLabel] != current.Labels[FleetLabel] || got.Status.Phase != current.Status.Phase {
				t.Fatalf("resume deleted or mutated a changed claim: current=%+v, got=%+v", current, got)
			}
			if _, present := x.podUIDs()[p.Name]; present {
				t.Fatal("aborted disk replacement left its terminating Pod pinned")
			}
		})
	}
}

func TestReliabilityPersistentReplacementHoldReleasedDuringLifecycleChanges(t *testing.T) {
	for _, lifecycle := range []string{"paused", "fleet-deleting", "scaled-in"} {
		t.Run(lifecycle, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			recordReplacementIntent(t, x, p, c, true)
			switch lifecycle {
			case "paused":
				x.edit(func(f *fleet.CelldFleet) { f.Spec.Maintenance = &fleet.MaintenanceSpec{Paused: true} })
			case "fleet-deleting":
				if err := x.r.Delete(t.Context(), x.f); err != nil {
					t.Fatal(err)
				}
			case "scaled-in":
				sts := x.workload().(*appsv1.StatefulSet)
				sts.Spec.Replicas = new(int32(1))
				if err := x.r.Update(t.Context(), sts); err != nil {
					t.Fatal(err)
				}
			}
			restartReplacementReconciler(x, x.r.Client)
			reconcileReplacementOnce(t, x)
			if _, present := x.podUIDs()[p.Name]; present {
				t.Fatal("lifecycle change stranded a replacement finalizer")
			}
			claim := x.claim(c.Name)
			if claim.UID != c.UID || (lifecycle != "scaled-in" && !claim.DeletionTimestamp.IsZero()) {
				t.Fatal("paused/deleting early cleanup started a new disk deletion")
			}
		})
	}
}

func TestReliabilityPersistentMalformedReplacementHoldsReleaseSafely(t *testing.T) {
	for _, invalid := range []string{"missing-annotation", "invalid-json", "missing-claim-identity", "committed-live-pod"} {
		t.Run(invalid, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			p = recordReplacementIntent(t, x, p, c, false)
			switch invalid {
			case "missing-annotation":
				delete(p.Annotations, replacementAnnotation)
			case "invalid-json":
				p.Annotations[replacementAnnotation] = "{"
			case "missing-claim-identity":
				p.Annotations[replacementAnnotation] = `{"committed":true}`
			case "committed-live-pod":
				setReplacementIntent(p, replacementIntent{ClaimUID: c.UID, ClaimResourceVersion: c.ResourceVersion, Committed: true})
			}
			if err := x.r.Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if handled, err := x.r.resumeReplacements(t.Context(), x.f); err != nil || !handled {
				t.Fatalf("malformed hold did not release: handled=%v, err=%v", handled, err)
			}
			if got := x.claim(c.Name); got.UID != c.UID || !got.DeletionTimestamp.IsZero() {
				t.Fatal("an invalid replacement marker authorized disk deletion")
			}
			if got := x.pod(p.Name); got.UID != p.UID || !got.DeletionTimestamp.IsZero() || slices.Contains(got.Finalizers, replacementFinalizer) || got.Annotations[replacementAnnotation] != "" || got.Labels[replacementFleetLabel] != "" {
				t.Fatal("malformed replacement cleanup deleted or pinned the live member")
			}
		})
	}
}

func TestReliabilityPersistentPreparedReplacementCleanupBeforeRuntimeValidation(t *testing.T) {
	x, p, c := replacementProtocolFixture(t)
	recordReplacementIntent(t, x, p, c, false)
	x.edit(func(f *fleet.CelldFleet) { f.Spec.RuntimeImage = "unqualified.example/celld:latest" })
	restartReplacementReconciler(x, x.r.Client)
	reconcileReplacementOnce(t, x)
	if claim := x.claim(c.Name); claim.UID != c.UID || !claim.DeletionTimestamp.IsZero() {
		t.Fatal("runtime validation bypass erased the prepared member's disk")
	}
	if current := x.pod(p.Name); slices.Contains(current.Finalizers, replacementFinalizer) || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" || !current.DeletionTimestamp.IsZero() {
		t.Fatal("runtime validation stranded the uncommitted replacement hold")
	}
}

func TestReliabilityPersistentHeldReplacementResumesBeforeForceDeletion(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			p = recordReplacementIntent(t, x, p, c, committed)
			if !committed {
				if err := x.r.Delete(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			x.clock = x.clock.Add(terminationMargin + time.Hour)
			base := x.r.Client
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, pod := obj.(*corev1.Pod); pod {
						t.Fatal("generic force deletion ran before resolving the replacement hold")
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			if _, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); err != nil {
				t.Fatal(err)
			}
			x.r.Client = base
			if _, present := x.podUIDs()[p.Name]; present {
				t.Fatal("replacement resume left its aged Pod pinned")
			}
			if deleting := !x.claim(c.Name).DeletionTimestamp.IsZero(); deleting != committed {
				t.Fatalf("resume chose wrong disk result: terminating=%v, committed=%v", deleting, committed)
			}
		})
	}
}

func TestReliabilityPersistentLostClaimWaitsForLivePodHold(t *testing.T) {
	for _, podState := range []string{"absent", "terminating"} {
		t.Run(podState, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			c.Status.Phase = corev1.ClaimLost
			if err := x.r.Status().Update(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			if podState == "terminating" {
				p.Finalizers = []string{"other.example/hold"}
				if err := x.r.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			if err := x.r.Delete(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			base := x.r.Client
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					t.Fatal("lost disk was deleted without holding a live member Pod")
					return nil
				},
			})
			if action, _, _, err := x.r.healMembers(t.Context(), x.f, x.workload().(*appsv1.StatefulSet), true); err != nil || action != "" {
				t.Fatalf("lost member did not wait for a live name hold: action=%q, err=%v", action, err)
			}
			x.r.Client = base
			if got := x.claim(c.Name); got.UID != c.UID || !got.DeletionTimestamp.IsZero() {
				t.Fatal("lost claim was erased before the Pod name could be held")
			}
		})
	}
}

func TestReliabilityPersistentReplacementRejectsInvalidClaimBindings(t *testing.T) {
	for _, invalid := range []string{"claim-name", "claim-namespace", "claim-fleet", "claim-fleet-missing", "claim-uid-missing", "claim-version-missing", "pod-fleet-missing"} {
		t.Run(invalid, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			candidate := c.DeepCopy()
			switch invalid {
			case "claim-name":
				candidate.Name = "data-another-member"
			case "claim-namespace":
				candidate.Namespace = "another-namespace"
			case "claim-fleet":
				candidate.Labels[FleetLabel] = "another-fleet"
			case "claim-fleet-missing":
				delete(candidate.Labels, FleetLabel)
			case "claim-uid-missing":
				candidate.UID = ""
			case "claim-version-missing":
				candidate.ResourceVersion = ""
			case "pod-fleet-missing":
				delete(p.Labels, FleetLabel)
				if err := x.r.Update(t.Context(), p); err != nil {
					t.Fatal(err)
				}
				p = x.pod(p.Name)
			}
			beforePod, beforeClaim := p.DeepCopy(), c.DeepCopy()
			base := x.r.Client
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					t.Fatal("invalid claim binding mutated a member before rejection")
					return nil
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					t.Fatal("invalid claim binding deleted a member or claim before rejection")
					return nil
				},
			})
			err := x.r.replaceMember(t.Context(), candidate, p)
			x.r.Client = base
			if !apierrors.IsConflict(err) {
				t.Fatalf("invalid claim binding was not rejected: %v", err)
			}
			if got := x.pod(p.Name); !equality.Semantic.DeepEqual(beforePod, got) {
				t.Fatalf("invalid binding changed the Pod: before=%+v, after=%+v", beforePod, got)
			}
			if got := x.claim(c.Name); !equality.Semantic.DeepEqual(beforeClaim, got) {
				t.Fatalf("invalid binding changed the claim: before=%+v, after=%+v", beforeClaim, got)
			}
		})
	}
}

func TestReliabilityPersistentReplacementRetainsDiskAfterPodOwnerDrift(t *testing.T) {
	for _, change := range []string{"uid", "name", "kind", "api-version", "missing"} {
		t.Run(change, func(t *testing.T) {
			x, p, c := replacementProtocolFixture(t)
			p.Finalizers = []string{"other.example/retain"}
			if err := x.r.Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			p = x.pod(p.Name)
			base := x.r.Client
			injected := false
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
						t.Fatal("changed Pod owner authorized disk deletion")
					}
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					current := &corev1.Pod{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
						return err
					}
					switch change {
					case "uid":
						current.OwnerReferences[0].UID = "another-controller"
					case "name":
						current.OwnerReferences[0].Name = "another-workload"
					case "kind":
						current.OwnerReferences[0].Kind = "ReplicaSet"
					case "api-version":
						current.OwnerReferences[0].APIVersion = "apps/v1beta1"
					case "missing":
						current.OwnerReferences = nil
					}
					if err := c.Update(ctx, current); err != nil {
						return err
					}
					injected = true
					return nil
				},
			})
			err := x.r.replaceMember(t.Context(), c, p)
			x.r.Client = base
			if !injected || !apierrors.IsConflict(err) {
				t.Fatalf("owner drift did not reject replacement: injected=%v, err=%v", injected, err)
			}
			if current := x.claim(c.Name); !equality.Semantic.DeepEqual(c, current) {
				t.Fatal("owner drift changed the retained claim")
			}
			current := x.pod(p.Name)
			if !slices.Equal(current.Finalizers, []string{"other.example/retain"}) || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" {
				t.Fatal("owner drift did not clean its own hold while preserving the foreign finalizer")
			}
		})
	}
}

func TestReliabilityPersistentReplacementConcurrentAbortRetainsDisk(t *testing.T) {
	for _, window := range []string{"before-pod-delete", "before-commit-update"} {
		t.Run(window, func(t *testing.T) {
			x, p, claim := replacementProtocolFixture(t)
			p.Finalizers = []string{"other.example/retain"}
			if err := x.r.Update(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			p = x.pod(p.Name)
			base := x.r.Client
			other := &Reconciler{Client: base, Options: x.r.Options, NetworkPolicyEnforced: true}
			injected := false
			abort := func(ctx context.Context) error {
				handled, err := other.resumeReplacements(ctx, x.f)
				if !handled || err != nil {
					t.Fatalf("second reconciler did not abort prepared intent: handled=%v, err=%v", handled, err)
				}
				injected = true
				return err
			}
			x.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if p, pod := obj.(*corev1.Pod); pod && window == "before-commit-update" && !injected {
						if intent, ok := replacementIntentOf(p); ok && intent.Committed {
							if err := abort(ctx); err != nil {
								return err
							}
						}
					}
					return c.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, pvc := obj.(*corev1.PersistentVolumeClaim); pvc {
						t.Fatal("concurrently aborted intent authorized PVC deletion")
					}
					if window == "before-pod-delete" && !injected {
						if err := abort(ctx); err != nil {
							return err
						}
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			err := x.r.replaceMember(t.Context(), claim, p)
			x.r.Client = base
			if !injected || !apierrors.IsConflict(err) {
				t.Fatalf("concurrent abort did not reject stale write: injected=%v, err=%v", injected, err)
			}
			if current := x.claim(claim.Name); !equality.Semantic.DeepEqual(claim, current) {
				t.Fatal("concurrent abort changed the retained claim")
			}
			current := x.pod(p.Name)
			if !slices.Equal(current.Finalizers, []string{"other.example/retain"}) || current.Annotations[replacementAnnotation] != "" || current.Labels[replacementFleetLabel] != "" {
				t.Fatal("concurrent abort lost foreign metadata or resurrected its replacement hold")
			}
		})
	}
}

func TestReliabilityPersistentReplacementHoldReleasedWhileStatefulSetDeletes(t *testing.T) {
	x, p, c := replacementProtocolFixture(t)
	recordReplacementIntent(t, x, p, c, true)
	sts := x.workload().(*appsv1.StatefulSet)
	sts.Finalizers = []string{"other.example/retain-workload"}
	if err := x.r.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	if err := x.r.Delete(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	reconcileReplacementOnce(t, x)
	if current := x.claim(c.Name); !equality.Semantic.DeepEqual(c, current) {
		t.Fatal("deleting StatefulSet cleanup started a new claim deletion")
	}
	if _, present := x.podUIDs()[p.Name]; present {
		t.Fatal("replacement hold blocked StatefulSet foreground cleanup")
	}
}
