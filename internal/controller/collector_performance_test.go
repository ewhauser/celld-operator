package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type collectorRoundTripper func(*http.Request) (*http.Response, error)

func (f collectorRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// collectorInventory models cached reads without making fake-client JSON
// serialization dominate the collector's CPU and allocation profile.
type collectorInventory struct {
	client.Client
	pods  corev1.PodList
	index map[string]int
}

func (c *collectorInventory) List(_ context.Context, out client.ObjectList, _ ...client.ListOption) error {
	c.pods.DeepCopyInto(out.(*corev1.PodList))
	return nil
}

func (c *collectorInventory) Get(_ context.Context, key client.ObjectKey, out client.Object, _ ...client.GetOption) error {
	i, ok := c.index[key.Name]
	if !ok {
		return errors.New("pod not found")
	}
	c.pods.Items[i].DeepCopyInto(out.(*corev1.Pod))
	return nil
}

func collectorPerformanceFixture(tb testing.TB, n int) (*Collector, *fleet.CelldFleet) {
	tb.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	f := fixture("alpha", "bucket-alpha", "Bucket")
	f.Spec.Capacity = &fleet.CapacityPolicy{}
	f.Default()
	inventory := &collectorInventory{index: make(map[string]int, n)}
	responses := make(map[string]string, n+1)
	raw, err := os.ReadFile("../runtime/controlplane/testdata/state.json")
	if err != nil {
		tb.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		tb.Fatal(err)
	}
	state["node_load"].(map[string]any)["sampled_ms"] = now.UnixMilli()
	raw, err = json.Marshal(state)
	if err != nil {
		tb.Fatal(err)
	}
	responses["/state"] = string(raw)
	for i := range n {
		name := fmt.Sprintf("alpha-%03d", n-i-1)
		inventory.index[name] = i
		inventory.pods.Items = append(inventory.pods.Items, corev1.Pod{Name: name, Namespace: f.Namespace, UID: types.UID(name), Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1", ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: "container-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Hour))}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}})
		responses["/apis/metrics.k8s.io/v1beta1/namespaces/"+f.Namespace+"/pods/"+name] = fmt.Sprintf(`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetrics","metadata":{"name":%q,"namespace":%q},"timestamp":%q,"window":"15s","containers":[{"name":"celld","usage":{"cpu":"123m","memory":"100Mi"}}]}`, name, f.Namespace, now.Format(time.RFC3339))
	}
	transport := collectorRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, ok := responses[req.URL.Path]
		if !ok {
			return nil, errors.New("unexpected collector request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	c, err := NewCollector(inventory, &rest.Config{Host: "http://metrics.test", Transport: transport, QPS: -1})
	if err != nil {
		tb.Fatal(err)
	}
	c.now = func() time.Time { return now }
	c.runtime = controlplane.New(transport)
	return c, f
}

func BenchmarkCollector(b *testing.B) {
	for _, n := range []int{3, 100} {
		b.Run(fmt.Sprintf("replicas=%d", n), func(b *testing.B) {
			c, f := collectorPerformanceFixture(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				o := c.Collect(b.Context(), f)
				if !o.Complete || len(o.Samples) != n || o.Samples[0].CPU != 123 || o.Samples[0].RuntimeAt.IsZero() {
					b.Fatal("incomplete benchmark observation")
				}
			}
		})
	}
}

type countedMetricsBody struct {
	io.Reader
	read   int
	closed bool
}

func (r *countedMetricsBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func (r *countedMetricsBody) Close() error {
	r.closed = true
	return nil
}

func TestCollectorBoundsMetricsResponseBeforeAllocation(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) { testCollectorBoundsMetricsResponse(t, code) })
	}
}

func testCollectorBoundsMetricsResponse(t *testing.T, code int) {
	t.Helper()
	c, f := collectorPerformanceFixture(t, 1)
	body := &countedMetricsBody{Reader: strings.NewReader(strings.Repeat(" ", 4<<20))}
	transport := collectorRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: req}, nil
	})
	metrics, err := NewCollector(c.client, &rest.Config{Host: "http://metrics.test", Transport: transport, QPS: -1})
	if err != nil {
		t.Fatal(err)
	}
	c.metrics = metrics.metrics
	o := c.Collect(t.Context(), f)
	if !body.closed || body.read > (1<<20)+1 {
		t.Fatalf("metrics response was not bounded and closed: read=%d closed=%t", body.read, body.closed)
	}
	if !o.Samples[0].MetricsAt.IsZero() || o.Samples[0].RuntimeAt.IsZero() {
		t.Fatal("oversized metrics accepted or runtime observation discarded")
	}
}

func BenchmarkCollectorOversizedMetrics(b *testing.B) {
	c, f := collectorPerformanceFixture(b, 1)
	oversize := strings.Repeat(" ", 4<<20)
	transport := collectorRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(oversize)), Request: req}, nil
	})
	metrics, err := NewCollector(c.client, &rest.Config{Host: "http://metrics.test", Transport: transport, QPS: -1})
	if err != nil {
		b.Fatal(err)
	}
	c.metrics = metrics.metrics
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = c.Collect(b.Context(), f)
	}
}
