package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unready marks one replica unready, as a replacement Pod whose launcher never
// starts its child would be, without any current operation.
func (x *operationFixture) unready(name string) {
	x.t.Helper()
	ctx := x.t.Context()
	p := &corev1.Pod{}
	if err := x.r.Get(ctx, client.ObjectKey{Namespace: x.f.Namespace, Name: name}, p); err != nil {
		x.t.Fatal(err)
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := x.r.Status().Update(ctx, p); err != nil {
		x.t.Fatal(err)
	}
	w := x.workload()
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		w.Status.ReadyReplicas--
	case *appsv1.Deployment:
		w.Status.ReadyReplicas--
	}
	if err := x.r.Status().Update(ctx, w); err != nil {
		x.t.Fatal(err)
	}
}

func TestUnreadyReplicaReportsBlockedLauncher(t *testing.T) {
	for _, profile := range []string{"PersistentFleet"} {
		for _, c := range []struct {
			name, reason, message string
			launcher              func(launcher.State) (launcher.State, error)
		}{
			{name: "retired disk", reason: "DiskRetired", message: "Pod alpha-0 cannot start: its disk was retired", launcher: func(s launcher.State) (launcher.State, error) {
				s.Phase, s.Error, s.PID = "Blocked", launcher.ErrDiskRetired.Error(), 0
				return s, nil
			}},
			{name: "other block", reason: "LauncherBlocked", message: "Pod alpha-0 launcher is blocked and will not start the runtime: cross-host disk reuse requires disk-policy cutover", launcher: func(s launcher.State) (launcher.State, error) {
				s.Phase, s.Error, s.PID = "Blocked", "cross-host disk reuse requires disk-policy cutover", 0
				return s, nil
			}},
			{name: "starting", reason: "Provisioning", launcher: func(s launcher.State) (launcher.State, error) {
				s.Phase, s.PID = "WaitingForExclusiveVolume", 0
				return s, nil
			}},
			{name: "unreachable", reason: "Provisioning", launcher: func(launcher.State) (launcher.State, error) {
				return launcher.State{}, errors.New("connection refused")
			}},
		} {
			t.Run(profile+"/"+c.name, func(t *testing.T) {
				x := newOperationFixture(t, profile)
				x.unready("alpha-0")
				requests := 0
				x.r.launcherCall = func(ctx context.Context, f *fleet.CelldFleet, p *corev1.Pod, operation, generation string) (launcher.State, error) {
					if operation != "" || generation != "" {
						t.Fatal("status diagnosis issued a launcher operation")
					}
					requests++
					if p.Name != "alpha-0" {
						t.Fatalf("ready replica %s was queried", p.Name)
					}
					return c.launcher(x.states[p.Name])
				}
				got := reconcile(t, x.r, x.f)
				ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
				blocked := meta.FindStatusCondition(got.Status.Conditions, "Blocked")
				if ready == nil || ready.Reason != c.reason || !strings.Contains(ready.Message, c.message) {
					t.Fatalf("want %s %q, got %+v", c.reason, c.message, ready)
				}
				if want := c.reason != "Provisioning"; blocked == nil || (blocked.Status == metav1.ConditionTrue) != want {
					t.Fatalf("Blocked=%v expected for %s, got %+v", want, c.reason, blocked)
				}
				if requests != 1 {
					t.Fatalf("launcher reads %d, want exactly one for the unready replica", requests)
				}
				if x.state().Operation != nil {
					t.Fatal("status diagnosis started an operation")
				}
			})
		}
	}
}
