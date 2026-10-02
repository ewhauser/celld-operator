package controller

import (
	"context"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func reliabilityDeleteOwnershipRace(t *testing.T, r *Reconciler, f *fleet.CelldFleet) {
	t.Helper()
	reconcile(t, r, f)
	base := r.Client
	if err := base.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	raced := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*appsv1.Deployment); ok && !raced {
			raced = true
			other := &appsv1.Deployment{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
				return err
			}
			other.Labels[FleetLabel] = "another-fleet-uid"
			other.Annotations = map[string]string{"example.com/concurrent-owner": "keep"}
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.Delete(ctx, obj, opts...)
	}})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
	if !raced || !apierrors.IsConflict(err) {
		t.Fatalf("deletion did not reject ownership changed after verification: raced=%t err=%v", raced, err)
	}
	r.Client = base
	w := &appsv1.Deployment{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
		t.Fatalf("concurrent owner's workload was deleted: %v", err)
	}
	if !w.DeletionTimestamp.IsZero() || w.Labels[FleetLabel] != "another-fleet-uid" {
		t.Fatal("deletion reached a workload whose ownership changed")
	}
	got := reconcile(t, r, f)
	reason(t, got, "DeletionBlocked")
	if !controllerutil.ContainsFinalizer(got, Finalizer) {
		t.Fatal("blocked deletion released the fleet finalizer")
	}
	res := envReservation(t, r, f)
	if res.Spec.FleetUID != string(f.UID) || !res.DeletionTimestamp.IsZero() {
		t.Fatal("blocked deletion changed the permanent reservation")
	}
}

func TestReliabilityDeletionOwnershipCAS(t *testing.T) {
	f := fixture("alpha", "reliability-alpha", "Bucket")
	reliabilityDeleteOwnershipRace(t, setup(t, f), f)
}

func TestEnvtestReliabilityDeletionOwnershipCAS(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	reliabilityDeleteOwnershipRace(t, r, x.fleet)
}

// These races exercise real resourceVersions. The competing writer runs after
// verification and before the operator's Update reaches the API server.
func TestEnvtestReliabilityWorkloadOwnershipCAS(t *testing.T) {
	for _, profile := range []string{"Deployment", "Ordered", "PersistentFleet"} {
		for _, foreign := range []bool{false, true} {
			name := profile + "/metadata"
			if foreign {
				name = profile + "/ownership"
			}
			t.Run(name, func(t *testing.T) {
				kind := "Bucket"
				if profile == "PersistentFleet" {
					kind = profile
				}
				r, x := envtestSetupWith(t, kind, func(f *fleet.CelldFleet) {
					if profile == "Ordered" {
						f.Spec.BucketWorkload = profile
					}
				})
				x.provision(t, r)
				f := &fleet.CelldFleet{}
				if err := r.Get(t.Context(), client.ObjectKeyFromObject(x.fleet), f); err != nil {
					t.Fatal(err)
				}
				f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "force-template-update"}
				if err := r.Update(t.Context(), f); err != nil {
					t.Fatal(err)
				}
				base := r.Client
				raced := false
				r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, workload := obj.(*appsv1.Deployment); !workload {
						_, workload = obj.(*appsv1.StatefulSet)
						if !workload {
							return c.Update(ctx, obj, opts...)
						}
					}
					if !raced {
						raced = true
						other := emptyObject(obj)
						if err := c.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
							return err
						}
						annotations := other.GetAnnotations()
						if annotations == nil {
							annotations = map[string]string{}
						}
						annotations["example.com/concurrent-writer"] = "keep"
						other.SetAnnotations(annotations)
						if foreign {
							other.GetLabels()[FleetLabel] = "another-fleet-uid"
						}
						if err := c.Update(ctx, other); err != nil {
							return err
						}
					}
					return c.Update(ctx, obj, opts...)
				}})
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
				if !raced || !apierrors.IsConflict(err) {
					t.Fatalf("stale workload update was not rejected: raced=%t err=%v", raced, err)
				}
				r.Client = base
				got := reconcile(t, r, f)
				w := emptyObject(workload(f, r.Options))
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
					t.Fatal(err)
				}
				if w.GetAnnotations()["example.com/concurrent-writer"] != "keep" {
					t.Fatal("retry discarded concurrent metadata")
				}
				if foreign {
					reason(t, got, "LifecycleBlocked")
					if w.GetLabels()[FleetLabel] != "another-fleet-uid" || matches(workload(f, r.Options), w) {
						t.Fatal("retry adopted or mutated a workload after its ownership changed")
					}
				} else if !matches(workload(f, r.Options), w) {
					t.Fatal("retry failed to converge after harmless concurrent metadata edit")
				}
			})
		}
	}
}

func TestEnvtestReliabilityStatusConflictPreservesNewSpec(t *testing.T) {
	r, x := envtestSetup(t, "Bucket")
	base := r.Client
	raced := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if sub == "status" && !raced {
			raced = true
			other := &fleet.CelldFleet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
				return err
			}
			other.Spec.Replicas = 4
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}})
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(x.fleet)})
	if !raced || !apierrors.IsConflict(err) {
		t.Fatalf("stale status patch was not rejected: raced=%t err=%v", raced, err)
	}
	r.Client = base
	got := reconcile(t, r, x.fleet)
	if got.Spec.Replicas != 4 || got.Status.DesiredReplicas != 4 || got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("retry lost concurrent replica intent or projected stale generation: spec=%d status=%+v", got.Spec.Replicas, got.Status)
	}
	w := &appsv1.Deployment{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(got), w); err != nil {
		t.Fatal(err)
	}
	if replicas(w) != 4 || meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Fatal("retry did not apply current spec or claimed unobserved workload readiness")
	}
}

// Real API defaulting and identity assignment are preserved while a transport
// fault discards a successful write response. Recovery must re-read committed
// state rather than create another workload or transfer the reservation.
func TestEnvtestReliabilityCommittedWriteReplay(t *testing.T) {
	for _, profile := range []string{"Deployment", "Ordered", "PersistentFleet"} {
		for _, target := range []string{"finalizer", "reservation", "workload", "status", "capacity-state"} {
			t.Run(profile+"/"+target, func(t *testing.T) {
				kind := "Bucket"
				if profile == "PersistentFleet" {
					kind = profile
				}
				r, x := envtestSetupWith(t, kind, func(f *fleet.CelldFleet) {
					if profile == "Ordered" {
						f.Spec.BucketWorkload = profile
					}
				})
				f := x.fleet
				if target == "capacity-state" {
					x.provision(t, r)
					f = enableCapacity(t, r, f, "Shadow")
				}
				base := r.Client
				hit := false
				lose := func(err error) error {
					if err == nil && !hit {
						hit = true
						return apierrors.NewTimeoutError("API committed write but transport lost its response", 1)
					}
					return err
				}
				r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						err := c.Create(ctx, obj, opts...)
						switch obj.(type) {
						case *fleet.CelldStorageReservation:
							if target == "reservation" {
								return lose(err)
							}
						case *appsv1.Deployment, *appsv1.StatefulSet:
							if target == "workload" {
								return lose(err)
							}
						}
						return err
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
						err := c.Patch(ctx, obj, p, opts...)
						if target == "finalizer" {
							return lose(err)
						}
						return err
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						err := c.Update(ctx, obj, opts...)
						if _, reservation := obj.(*fleet.CelldStorageReservation); reservation && target == "capacity-state" {
							return lose(err)
						}
						return err
					},
					SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
						err := c.SubResource(sub).Patch(ctx, obj, p, opts...)
						if target == "status" && sub == "status" {
							return lose(err)
						}
						return err
					},
				})
				_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
				if !hit || !apierrors.IsTimeout(err) {
					t.Fatalf("committed-write fault was not delivered: hit=%t err=%v", hit, err)
				}
				// A new controller has none of the old response objects in memory.
				r = &Reconciler{Client: base, Options: r.Options, NetworkPolicyEnforced: true}
				var workloadUID string
				w := emptyObject(workload(f, r.Options))
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err == nil {
					workloadUID = string(w.GetUID())
				} else if !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
				converged := false
				for range 8 {
					if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)}); err != nil {
						t.Fatal(err)
					}
					reliabilitySafety(t, r, f, true)
					if reliabilityManifestsMatch(t, r, f) {
						converged = true
						break
					}
				}
				if !converged {
					t.Fatal("replay failed to converge after committed response loss")
				}
				if err := base.Get(t.Context(), client.ObjectKeyFromObject(f), w); err != nil {
					t.Fatal(err)
				}
				if workloadUID != "" && string(w.GetUID()) != workloadUID {
					t.Fatal("replay replaced a workload whose creation had committed")
				}
			})
		}
	}
}
