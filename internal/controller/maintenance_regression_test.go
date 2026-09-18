package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMaintenanceWithdrawalSchedulesContinuation(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, kind := range []string{"Restart", "Upgrade"} {
			t.Run(profile+"/"+kind, func(t *testing.T) {
				r, f := lifecycleSetup(t, profile)
				f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) {
					if kind == "Restart" {
						f.Spec.Maintenance = &fleet.MaintenanceSpec{RestartToken: "requested"}
					} else {
						f.Spec.RuntimeImage = "ghcr.io/denoland/celld@sha256:" + strings.Repeat("a", 64)
					}
				})
				reconcile(t, r, f)
				f = editMaintenance(t, r, f, func(f *fleet.CelldFleet) { f.Spec.Maintenance = nil; f.Spec.RuntimeImage = ""; f.Spec.Replicas = 5 })
				// A new leader has no delayed queue entries from before the withdrawal.
				r = &Reconciler{Client: r.Client, Options: r.Options, NetworkPolicyEnforced: true}
				for range 8 {
					result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f)})
					if err != nil {
						t.Fatal(err)
					}
					j := getJournal(t, r, f)
					if j.Applied == 5 && j.Operation == nil && j.Request == nil {
						return
					}
					if result.RequeueAfter <= 0 {
						t.Fatal("withdrawal stalled pending capacity without a scheduled continuation")
					}
				}
				t.Fatal("scheduled reconciles did not complete the pending addition")
			})
		}
	}
}

func TestMaintenanceDeletionReportsOnceAndStabilizes(t *testing.T) {
	for _, state := range []string{"idle", "unissued", "recovering"} {
		t.Run(state, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", "PersistentFleet")
			f.Spec.Placement.AZCount = 1
			f.Spec.Placement.Zones = []string{"us-east-1a"}
			r := setup(t, f)
			e := &localEvidence{now: time.Now(), stopped: true}
			r.Options.LocalTest = true
			r.localLifecycle = e
			reconcile(t, r, f)
			reconcile(t, r, f)
			switch state {
			case "unissued":
				f = desiredCount(t, r, f, 5)
				reconcile(t, r, f)
			case "recovering":
				f = desiredCount(t, r, f, 2)
				reconcile(t, r, f)
				reconcile(t, r, f)
				e.incomplete = true
			}
			if err := r.Delete(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			patches := 0
			base := r.Client
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "status" {
					patches++
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			}})
			got := reconcile(t, r, f)
			if patches > 1 {
				t.Fatalf("deletion wrote %d different statuses in one reconcile", patches)
			}
			expected := "DeletionBlocked"
			if state == "recovering" {
				expected = "RecoveryBlocked"
			}
			reason(t, got, expected)
			patches = 0
			for range 3 {
				reason(t, reconcile(t, r, f), expected)
			}
			if patches != 0 {
				t.Fatalf("unchanged deletion caused %d status writes", patches)
			}
			j := getJournal(t, r, f)
			if j.Request == nil || j.Request.Kind != "Delete" || len(j.Claims) != 3 {
				t.Fatal("deletion lost retained intent or storage identities")
			}
			if state == "recovering" && (j.Operation == nil || j.Operation.Phase != "Recovering" || j.Applied != 3) {
				t.Fatal("uncertain recovery was completed or discarded")
			}
		})
	}
}
