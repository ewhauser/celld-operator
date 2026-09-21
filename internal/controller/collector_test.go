package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCollectorRuntimeAndMetrics(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		t.Run(profile, func(t *testing.T) { testCollectorRuntimeAndMetrics(t, profile) })
	}
}
func testCollectorRuntimeAndMetrics(t *testing.T, profile string) {
	for _, scenario := range []string{"complete", "unknown shutdown schema", "denied", "missing cpu", "stale", "future", "unknown state", "restarted", "oversize", "redirect", "bad window", "wrong identity", "unready", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			f := fixture("alpha", "bucket-alpha", profile)
			f.Spec.Capacity = &fleet.CapacityPolicy{}
			f.Default()
			p := &corev1.Pod{Name: "alpha-pod", Namespace: f.Namespace, UID: "pod-uid", Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			if scenario == "unready" {
				p.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			r := setup(t, f, p)
			raw, err := os.ReadFile("../runtime/controlplane/testdata/state.json")
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state["node_load"].(map[string]any)["sampled_ms"] = now.UnixMilli()
			usage := map[string]string{"cpu": "123m", "memory": "100Mi"}
			timestamp := now
			window := "15s"
			name := p.Name
			switch scenario {
			case "unknown shutdown schema":
				state["shutdown"] = map[string]any{"schema_version": 2, "capabilities": "future schema"}
			case "missing cpu":
				delete(usage, "cpu")
			case "stale":
				timestamp = now.Add(-time.Minute)
			case "future":
				timestamp = now.Add(time.Second)
			case "bad window":
				window = "0s"
			case "wrong identity":
				name = "different"
			case "unknown state":
				delete(state, "node_load")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					t.Error("collector performed mutation")
				}
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/state" {
					if scenario == "oversize" {
						_, _ = w.Write([]byte(strings.Repeat(" ", 1024*1024+1)))
						return
					}
					if scenario == "redirect" {
						w.Header().Set("Location", "/mutation")
						w.WriteHeader(http.StatusFound)
						return
					}
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				if req.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/fleets/pods/alpha-pod" {
					t.Errorf("unexpected endpoint %s", req.URL.Path)
				}
				if scenario == "denied" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if scenario == "restarted" {
					current := &corev1.Pod{}
					if err := r.Get(req.Context(), client.ObjectKeyFromObject(p), current); err != nil {
						t.Error(err)
					}
					current.Status.ContainerStatuses[0].ContainerID = "replacement"
					if err := r.Status().Update(req.Context(), current); err != nil {
						t.Error(err)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics", "metadata": map[string]string{"name": name, "namespace": p.Namespace}, "timestamp": timestamp.Format(time.RFC3339Nano), "window": window, "containers": []any{map[string]any{"name": "celld", "usage": usage}}})
			}))
			defer server.Close()
			c, err := NewCollector(r.Client, &rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			c.now = func() time.Time { return now }
			transport := &http.Transport{}
			c.runtime = controlplane.New(transport)
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()
			ctx := t.Context()
			if scenario == "canceled" {
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			}
			o := c.Collect(ctx, f)
			if len(o.Samples) != 1 {
				if scenario == "canceled" {
					return
				}
				t.Fatal(o)
			}
			s := o.Samples[0]
			if scenario == "complete" || scenario == "unready" {
				if s.CPU != 123 || s.MemoryMiB != 100 || s.MetricsAt.IsZero() || s.RuntimeAt.IsZero() || s.Ready != (scenario != "unready") || s.RuntimeMemoryMiB != 52 {
					t.Fatal(s)
				}
			} else {
				evaluated := capacity.Evaluate(*f.Spec.Capacity, capacity.State{}, o, 1)
				if evaluated.Decision.CoveredReplicas != 0 {
					t.Fatal("bad sample counted", scenario, s, evaluated)
				}
			}
		})
	}
}
