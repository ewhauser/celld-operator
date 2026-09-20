package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRuntimeLifecycleProfileSeams(t *testing.T) {
	for _, profile := range []string{"PersistentFleet", "Bucket"} {
		t.Run(profile, func(t *testing.T) {
			f := fixture("alpha", "bucket-alpha", profile)
			pod := &corev1.Pod{Name: "alpha-0", Namespace: f.Namespace, UID: "pod-uid", Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
			target, err := runtimeTarget(f, pod, "exact-generation")
			wantNode := pod.Name
			if profile == "Bucket" {
				wantNode = string(pod.UID)
			}
			if err != nil || target.Node != wantNode || target.Generation != "exact-generation" || target.IP != pod.Status.PodIP {
				t.Fatal(target, err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/state" || r.Method != http.MethodGet {
					t.Error("unexpected mutation", r.URL)
				}
				_, _ = fmt.Fprint(w, `{"shutdown":{"schema_version":1,"runtime_generation":"exact-generation","capabilities":{"strict_disk_removal":true},"control_only":true,"operation":{"operation_id":"op-1","expected_generation":"exact-generation","mode":"remove-disk","phase":"data_safe","blocker":null}}}`)
			}))
			defer server.Close()
			transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "127.0.0.1:8081" {
					return nil, fmt.Errorf("unexpected address %s", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}
			defer transport.CloseIdleConnections()
			var lifecycle controlplane.Lifecycle = controlplane.New(transport)
			status, err := lifecycle.RemovalStatus(t.Context(), target, "op-1")
			if err != nil || !status.DataSafe() {
				t.Fatal(status, err)
			}
			// Neither profile feeds this observation into existing removal machinery.
			// The old journal, launcher and recovery proofs remain separate requirements.
			for _, change := range []string{"foreign fleet", "foreign namespace", "unknown image", "deleted", "not running", "no UID", "service name"} {
				t.Run(change, func(t *testing.T) {
					bad := pod.DeepCopy()
					switch change {
					case "foreign fleet":
						bad.Labels[FleetLabel] = "other"
					case "foreign namespace":
						bad.Namespace = "other"
					case "unknown image":
						bad.Spec.Containers[0].Image = "unknown"
					case "deleted":
						now := metav1.Now()
						bad.DeletionTimestamp = &now
					case "not running":
						bad.Status.ContainerStatuses = nil
					case "no UID":
						bad.UID = ""
					case "service name":
						bad.Status.PodIP = "alpha.fleets.svc"
					}
					if _, err := runtimeTarget(f, bad, "exact-generation"); err == nil {
						t.Fatal("unsafe target accepted")
					}
				})
			}
		})
	}
}
