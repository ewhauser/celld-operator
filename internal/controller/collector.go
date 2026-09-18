package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type capacityCollector interface {
	Collect(context.Context, *fleet.CelldFleet) capacity.Observation
}

// Collector reads the pinned /state endpoint and individual metrics.k8s.io PodMetrics.
// It never calls runtime mutations, Pod proxy, Prometheus, or Kubernetes /scale.
type Collector struct {
	client  client.Client
	metrics rest.Interface
	http    *http.Client
	now     func() time.Time
}

func NewCollector(c client.Client, config *rest.Config) (*Collector, error) {
	k, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Collector{client: c, metrics: k.CoreV1().RESTClient(), now: time.Now, http: &http.Client{
		Timeout:       2 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, MaxConnsPerHost: 2, MaxIdleConns: 100, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}
func podIdentity(p *corev1.Pod) (string, time.Time) {
	if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Name != "celld" || p.Spec.Containers[0].Image != Image || !p.DeletionTimestamp.IsZero() {
		return "", time.Time{}
	}
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == "celld" && s.ContainerID != "" && s.State.Running != nil && !s.State.Running.StartedAt.IsZero() && p.UID != "" {
			return fmt.Sprintf("%s/%s/%d", p.UID, s.ContainerID, s.RestartCount), s.State.Running.StartedAt.Time
		}
	}
	return "", time.Time{}
}
func podReady(p *corev1.Pod) bool {
	return slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool { return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue })
}
func (c *Collector) Collect(ctx context.Context, f *fleet.CelldFleet) capacity.Observation {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := capacity.Observation{}
	pods := &corev1.PodList{}
	if err := c.client.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(labels(f)), client.Limit(101)); err != nil || pods.Continue != "" || len(pods.Items) > 100 {
		out.At = c.now()
		return out
	}
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	out.Samples = make([]capacity.Sample, len(pods.Items))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range min(8, len(pods.Items)) {
		wg.Go(func() {
			for i := range jobs {
				out.Samples[i] = c.sample(ctx, f, &pods.Items[i])
			}
		})
	}
	for i := range pods.Items {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	out.Complete = true
	out.At = c.now()
	return out
}
func (c *Collector) sample(ctx context.Context, f *fleet.CelldFleet, p *corev1.Pod) capacity.Sample {
	identity, started := podIdentity(p)
	s := capacity.Sample{Identity: identity, Ready: podReady(p)}
	if identity == "" || net.ParseIP(p.Status.PodIP) == nil {
		return s
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+net.JoinHostPort(p.Status.PodIP, "8081")+"/state", http.NoBody)
	if err == nil {
		response, requestErr := c.http.Do(req)
		if requestErr == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
			closeErr := response.Body.Close()
			received := c.now()
			adapter, adapterErr := v050.New(Image)
			if readErr == nil && closeErr == nil && adapterErr == nil && len(data) <= 1024*1024 {
				state, parseErr := adapter.ParseState(response.StatusCode, data, received, received, capacity.Seconds(f.Spec.Capacity.MaxAgeSeconds))
				if parseErr == nil && !state.SampledAt.Before(started) && state.RSSBytes <= 1<<50 && state.InUseBytes <= 1<<50 {
					s.RuntimeMemoryMiB = int64((max(state.RSSBytes, state.InUseBytes) + (1 << 20) - 1) / (1 << 20))
					s.RuntimeAt, s.RuntimeReceived = state.SampledAt, received
					s.Pressured = state.Pressured || !state.MemoryHeadroom
					s.Backlog = state.Draining || state.RebalancePaused || state.CapacityWaiting > 0 || state.ActivationWaiting > 0 || state.Restoring > 0
				}
			}
		}
	}
	cancel()
	metricsCtx, cancelMetrics := context.WithTimeout(ctx, 2*time.Second)
	defer cancelMetrics()
	data, err := c.metrics.Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + p.Namespace + "/pods/" + p.Name).Do(metricsCtx).Raw()
	received := c.now()
	if err == nil && len(data) <= 1024*1024 {
		var m struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta `json:"metadata"`
			Timestamp       metav1.Time       `json:"timestamp"`
			Window          metav1.Duration   `json:"window"`
			Containers      []struct {
				Name  string              `json:"name"`
				Usage corev1.ResourceList `json:"usage"`
			} `json:"containers"`
		}
		if json.Unmarshal(data, &m) == nil && m.APIVersion == "metrics.k8s.io/v1beta1" && m.Kind == "PodMetrics" && m.Metadata.Name == p.Name && m.Metadata.Namespace == p.Namespace && len(m.Containers) == 1 && m.Containers[0].Name == "celld" && m.Window.Duration > 0 && !m.Timestamp.Add(-m.Window.Duration).Before(started) {
			cpu, cpuOK := m.Containers[0].Usage[corev1.ResourceCPU]
			memory, memoryOK := m.Containers[0].Usage[corev1.ResourceMemory]
			// Reject negative and implausibly large quantities before integer conversion.
			if cpuOK && memoryOK && cpu.Sign() >= 0 && memory.Sign() >= 0 && cpu.AsApproximateFloat64() <= 1000000 && memory.AsApproximateFloat64() <= 1<<50 {
				s.CPU = cpu.MilliValue()
				s.MemoryMiB = (memory.Value() + (1 << 20) - 1) / (1 << 20)
				s.MetricsAt, s.MetricsReceived, s.Window = m.Timestamp.Time, received, m.Window.Duration
			}
		}
	}
	// Pod replacement, process restart, address change or readiness loss during reads
	// invalidates the entire sample. HTTP state is not a runtime-session certificate.
	current := &corev1.Pod{}
	if err := c.client.Get(ctx, client.ObjectKeyFromObject(p), current); err != nil {
		return capacity.Sample{Identity: identity}
	}
	currentID, _ := podIdentity(current)
	if currentID != identity || current.Status.PodIP != p.Status.PodIP || podReady(current) != s.Ready || current.Labels[FleetLabel] != string(f.UID) {
		return capacity.Sample{Identity: identity}
	}
	return s
}
