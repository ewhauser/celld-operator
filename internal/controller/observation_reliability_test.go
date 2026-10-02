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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type observationFaultTransport func(*http.Request) (*http.Response, error)

func (f observationFaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func observationResponse(req *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body, Request: req}
}

func observationRuntimeJSON(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../runtime/controlplane/testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func observationMetricsJSON(name, namespace string, at time.Time) map[string]any {
	return map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics", "metadata": map[string]string{"name": name, "namespace": namespace}, "timestamp": at.Format(time.RFC3339Nano), "window": "15s", "containers": []any{map[string]any{"name": "celld", "usage": map[string]string{"cpu": "10m", "memory": "10Mi"}}}}
}

func observationJSON(t *testing.T, v any) io.ReadCloser {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return io.NopCloser(strings.NewReader(string(raw)))
}

func observationCollector(t *testing.T, profile string, transport http.RoundTripper) (*Collector, *fleet.CelldFleet) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", profile)
	f.Spec.Capacity = &fleet.CapacityPolicy{Mode: "Automatic", MinReplicas: 1, MaxReplicas: 5}
	f.Default()
	objects := []client.Object{f}
	for i := range 3 {
		objects = append(objects, &corev1.Pod{Name: fmt.Sprintf("alpha-%d", i), Namespace: f.Namespace, UID: types.UID(fmt.Sprintf("pod-%d", i)), Labels: labels(f), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "celld", Image: fixtureRuntime}}}, Status: corev1.PodStatus{PodIP: fmt.Sprintf("127.0.0.%d", i+1), ContainerStatuses: []corev1.ContainerStatus{{Name: "celld", ContainerID: fmt.Sprintf("container-%d", i), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Hour))}}}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}})
	}
	r := setup(t, objects...)
	c, err := NewCollector(r.Client, &rest.Config{Host: "http://metrics.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	c.runtime = controlplane.New(transport)
	return c, f
}

// A fault in one member must invalidate the whole fleet's scaling evidence,
// without treating failed reads as low usage or suppressing healthy members.
// The next healthy read must recover without rebuilding the collector.
func TestObservationReliabilityPartialHTTPFaultRecovery(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, fault := range []string{"runtime reset", "runtime unavailable", "runtime truncated", "metrics denied", "metrics server error", "metrics truncated", "metrics malformed", "metrics stale", "metrics future", "metrics missing memory"} {
			t.Run(profile+"/"+fault, func(t *testing.T) {
				var phase atomic.Int32
				base := time.Now().Add(-time.Second).Truncate(time.Millisecond)
				transport := observationFaultTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet {
						t.Errorf("observation performed mutation: %s %s", req.Method, req.URL)
					}
					runtime := req.URL.Path == "/state"
					affected := phase.Load() == 1 && ((runtime && req.URL.Host == "127.0.0.2:8081") || (!runtime && strings.HasSuffix(req.URL.Path, "/alpha-1")))
					if affected {
						switch fault {
						case "runtime reset":
							if runtime {
								return nil, syscall.ECONNRESET
							}
						case "runtime unavailable":
							if runtime {
								return observationResponse(req, http.StatusServiceUnavailable, io.NopCloser(strings.NewReader(`{}`))), nil
							}
						case "runtime truncated", "metrics truncated":
							if runtime == strings.HasPrefix(fault, "runtime") {
								return observationResponse(req, http.StatusOK, &observationTruncatedBody{}), nil
							}
						case "metrics denied":
							if !runtime {
								return observationResponse(req, http.StatusForbidden, io.NopCloser(strings.NewReader(`{}`))), nil
							}
						case "metrics server error":
							if !runtime {
								return observationResponse(req, http.StatusInternalServerError, io.NopCloser(strings.NewReader(`{}`))), nil
							}
						case "metrics malformed":
							if !runtime {
								return observationResponse(req, http.StatusOK, io.NopCloser(strings.NewReader(`{"timestamp":`))), nil
							}
						}
					}
					at := base.Add(time.Duration(phase.Load()) * time.Millisecond)
					if runtime {
						state := observationRuntimeJSON(t)
						state["node_load"].(map[string]any)["sampled_ms"] = at.UnixMilli()
						return observationResponse(req, http.StatusOK, observationJSON(t, state)), nil
					}
					parts := strings.Split(req.URL.Path, "/")
					if affected {
						if fault == "metrics stale" {
							at = at.Add(-time.Hour)
						}
						if fault == "metrics future" {
							at = at.Add(time.Hour)
						}
					}
					metrics := observationMetricsJSON(parts[len(parts)-1], "fleets", at)
					if affected && fault == "metrics missing memory" {
						delete(metrics["containers"].([]any)[0].(map[string]any)["usage"].(map[string]string), "memory")
					}
					return observationResponse(req, http.StatusOK, observationJSON(t, metrics)), nil
				})
				c, f := observationCollector(t, profile, transport)
				initial := c.Collect(t.Context(), f)
				old := capacity.Evaluate(*f.Spec.Capacity, capacity.State{}, initial, 3)
				if old.Decision.CoveredReplicas != 3 || !capacity.LowDemand(*f.Spec.Capacity, initial, 3) {
					t.Fatalf("initial healthy coverage: %+v", old.Decision)
				}
				// Seed a qualified low window and make the next read a counted slot.
				old.LastObservation = initial.At.Add(-15 * time.Second)
				old.LowSince = initial.At.Add(-time.Hour)
				old.LowSamples = 100
				phase.Store(1)
				broken := c.Collect(t.Context(), f)
				evaluated := capacity.Evaluate(*f.Spec.Capacity, old, broken, 3)
				if len(broken.Samples) != 3 || !broken.Complete || evaluated.Decision.CoveredReplicas != 2 || evaluated.Actionable || !evaluated.LowSince.IsZero() || evaluated.LowSamples != 0 || capacity.LowDemand(*f.Spec.Capacity, broken, 3) {
					t.Fatalf("partial fault counted as safe scaling: %+v, samples %+v", evaluated, broken.Samples)
				}
				phase.Store(2)
				recovered := c.Collect(t.Context(), f)
				evaluated.LastObservation = recovered.At.Add(-15 * time.Second)
				evaluated = capacity.Evaluate(*f.Spec.Capacity, evaluated, recovered, 3)
				if evaluated.Decision.CoveredReplicas != 3 || !capacity.LowDemand(*f.Spec.Capacity, recovered, 3) || evaluated.Actionable || evaluated.LowSamples != 1 {
					t.Fatalf("healthy collection did not restart stabilization: %+v", evaluated)
				}
			})
		}
	}
}

type observationTruncatedBody struct{}

func (*observationTruncatedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (*observationTruncatedBody) Close() error             { return nil }

// Oversized responses must be stopped while reading. Checking len after Raw
// has already buffered the body does not protect the operator's memory budget.
func TestObservationReliabilityMetricsResponseBudget(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			const budget = 1 << 20
			var oversized atomic.Bool
			oversized.Store(true)
			body := &observationCountingBody{remaining: 8 * budget}
			transport := observationFaultTransport(func(req *http.Request) (*http.Response, error) {
				at := time.Now().Add(-time.Second)
				if req.URL.Path == "/state" {
					state := observationRuntimeJSON(t)
					state["node_load"].(map[string]any)["sampled_ms"] = at.UnixMilli()
					return observationResponse(req, http.StatusOK, observationJSON(t, state)), nil
				}
				if oversized.Load() && strings.HasSuffix(req.URL.Path, "/alpha-1") {
					return observationResponse(req, status, body), nil
				}
				parts := strings.Split(req.URL.Path, "/")
				return observationResponse(req, http.StatusOK, observationJSON(t, observationMetricsJSON(parts[len(parts)-1], "fleets", at))), nil
			})
			c, f := observationCollector(t, "Bucket", transport)
			o := c.Collect(t.Context(), f)
			if !body.closed || body.read > budget+1 {
				t.Fatalf("oversized metrics read %d bytes and closed=%t; budget is %d", body.read, body.closed, budget+1)
			}
			if s := capacity.Evaluate(*f.Spec.Capacity, capacity.State{}, o, 3); s.Actionable || s.Decision.CoveredReplicas != 2 {
				t.Fatalf("oversized response supplied capacity evidence: %+v", s.Decision)
			}
			oversized.Store(false)
			o = c.Collect(t.Context(), f)
			if s := capacity.Evaluate(*f.Spec.Capacity, capacity.State{}, o, 3); s.Decision.CoveredReplicas != 3 {
				t.Fatalf("collector did not recover after oversized body: %+v", s.Decision)
			}
		})
	}
}

type observationCountingBody struct {
	remaining, read int
	closed          bool
}

func (b *observationCountingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	for i := range n {
		p[i] = ' '
	}
	b.remaining -= n
	b.read += n
	return n, nil
}
func (b *observationCountingBody) Close() error { b.closed = true; return nil }

// Cancellation must interrupt a stalled transport, prevent incomplete reads
// from authorizing scaling, and leave later collections usable.
func TestObservationReliabilityCanceledCollectionRecovers(t *testing.T) {
	var stall atomic.Bool
	stall.Store(true)
	transport := observationFaultTransport(func(req *http.Request) (*http.Response, error) {
		if stall.Load() {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		at := time.Now().Add(-time.Second)
		if req.URL.Path == "/state" {
			state := observationRuntimeJSON(t)
			state["node_load"].(map[string]any)["sampled_ms"] = at.UnixMilli()
			return observationResponse(req, http.StatusOK, observationJSON(t, state)), nil
		}
		parts := strings.Split(req.URL.Path, "/")
		return observationResponse(req, http.StatusOK, observationJSON(t, observationMetricsJSON(parts[len(parts)-1], "fleets", at))), nil
	})
	c, f := observationCollector(t, "PersistentFleet", transport)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	o := c.Collect(ctx, f)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled collection took %v", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || capacity.LowDemand(*f.Spec.Capacity, o, 3) {
		t.Fatalf("deadline did not invalidate observation: %+v", o)
	}
	stall.Store(false)
	o = c.Collect(t.Context(), f)
	if s := capacity.Evaluate(*f.Spec.Capacity, capacity.State{}, o, 3); s.Decision.CoveredReplicas != 3 {
		t.Fatalf("collector did not recover after cancellation: %+v", s.Decision)
	}
}

// Long outages and repeated source snapshots must erase both high and low
// stabilization. Recovery earns a complete new window before either direction
// is actionable; a recommendation from before the fault cannot be reused.
func TestObservationReliabilityCapacityOutageRecovery(t *testing.T) {
	for _, profile := range []string{"Bucket", "PersistentFleet"} {
		for _, demand := range []string{"high", "low"} {
			for _, fault := range []string{"inventory unavailable", "missing member", "missing runtime", "missing metrics", "replayed source", "future metrics"} {
				t.Run(profile+"/"+demand+"/"+fault, func(t *testing.T) {
					x := newOperationFixture(t, profile)
					x.f = enableCapacity(t, x.r, x.f, "Automatic")
					x.f.Spec.Capacity.ScaleInStabilizationSeconds = 60
					window := capacity.Seconds(x.f.Spec.Capacity.ScaleInStabilizationSeconds)
					want := int32(2)
					if demand == "high" {
						x.cpu = 1000
						window = capacity.Seconds(x.f.Spec.Capacity.ScaleOutStabilizationSeconds)
						want = 4
					}
					j := &fleetState{Capacity: &capacity.State{LastManual: x.f.Spec.Replicas}}
					base := x.r.Collector
					var lastGood capacity.Observation
					broken := false
					x.r.Collector = collectorFunc(func(ctx context.Context, f *fleet.CelldFleet) capacity.Observation {
						o := base.Collect(ctx, f)
						if !broken {
							lastGood = o
							return o
						}
						switch fault {
						case "inventory unavailable":
							o.Complete, o.Samples = false, nil
						case "missing member":
							o.Samples = o.Samples[:2]
						case "missing runtime":
							o.Samples[0].RuntimeAt = time.Time{}
						case "missing metrics":
							o.Samples[0].MetricsAt = time.Time{}
						case "replayed source":
							for i := range o.Samples {
								o.Samples[i].RuntimeAt = lastGood.Samples[i].RuntimeAt
								o.Samples[i].MetricsAt = lastGood.Samples[i].MetricsAt
							}
						case "future metrics":
							o.Samples[0].MetricsAt = o.At.Add(time.Hour)
						}
						return o
					})
					start := x.clock
					for x.clock.Sub(start) < window {
						if target, automatic := x.r.capacityTarget(t.Context(), x.f, j, 3); target != 3 || automatic {
							t.Fatalf("premature healthy action: %d %t", target, automatic)
						}
						x.clock = x.clock.Add(15 * time.Second)
					}
					broken = true
					for range 8 {
						if target, automatic := x.r.capacityTarget(t.Context(), x.f, j, 3); target != 3 || automatic || j.Capacity.Actionable || !j.Capacity.HighSince.IsZero() || !j.Capacity.LowSince.IsZero() {
							t.Fatalf("fault retained scaling evidence: target=%d automatic=%t state=%+v", target, automatic, j.Capacity)
						}
						x.clock = x.clock.Add(15 * time.Second)
					}
					broken = false
					start = x.clock
					for x.clock.Sub(start) < window {
						if target, automatic := x.r.capacityTarget(t.Context(), x.f, j, 3); target != 3 || automatic {
							t.Fatalf("pre-fault stabilization survived recovery: target=%d automatic=%t state=%+v", target, automatic, j.Capacity)
						}
						x.clock = x.clock.Add(15 * time.Second)
					}
					if target, automatic := x.r.capacityTarget(t.Context(), x.f, j, 3); target != want || !automatic {
						t.Fatalf("healthy recovery never became actionable: target=%d automatic=%t state=%+v", target, automatic, j.Capacity)
					}
				})
			}
		}
	}
}
