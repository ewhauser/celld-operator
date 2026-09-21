package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeLauncher is an HTTP stand-in for cmd/celld-launcher. It authenticates
// requests with the real protocol and lets a test mutate the signed response to
// probe every rejection branch of the controller's client.
type fakeLauncher struct {
	key      []byte
	state    launcher.State
	calls    atomic.Int32
	lastReq  launcher.Request
	mutate   func(*launcher.Response)
	status   int
	redirect bool
	body     []byte
}

func (l *fakeLauncher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/v2" {
		http.NotFound(w, r)
		return
	}

	l.calls.Add(1)
	if l.redirect {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
		return
	}
	if l.status != 0 {
		http.Error(w, "unavailable", l.status)
		return
	}
	var req launcher.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !launcher.Verify(l.key, "request", req, r.Header.Get("X-Celld-MAC")) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	l.lastReq = req
	if l.body != nil {
		_, _ = w.Write(l.body)
		return
	}
	signed := struct {
		Nonce string
		State launcher.State
	}{req.Nonce, l.state}
	resp := launcher.Response{Nonce: req.Nonce, State: l.state, MAC: launcher.MAC(l.key, "response", signed)}
	if l.mutate != nil {
		l.mutate(&resp)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func launcherClientSetup(t *testing.T) (*Reconciler, *fleet.CelldFleet, *corev1.Pod, *fakeLauncher) {
	t.Helper()
	f := fixture("alpha", "bucket-alpha", "PersistentFleet")
	key := []byte(strings.Repeat("k", 32))
	secret := &corev1.Secret{Name: launcherSecretName(f), Namespace: f.Namespace, Labels: labels(f), Immutable: new(true), Data: map[string][]byte{"key": key}}
	res := &fleet.CelldStorageReservation{Name: reservationName(f), Annotations: map[string]string{launcherKeyDigest: digest(key)}, Spec: fleet.ReservationSpec{FleetUID: string(f.UID)}}
	pod := &corev1.Pod{Name: "alpha-2", Namespace: f.Namespace, UID: types.UID("pod-uid"), Spec: corev1.PodSpec{NodeName: "host-2"}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}}
	r := setup(t, f, secret, res, pod)
	r.Options.LauncherImage = "launcher@sha256:" + strings.Repeat("a", 64)
	fake := &fakeLauncher{key: key, state: launcher.State{PodUID: string(pod.UID), Node: pod.Name, Host: pod.Spec.NodeName, Invocation: "inv-1", Generation: "gen-1", Phase: "Draining", Operation: "op-1"}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	previous := launcherPort
	launcherPort = port
	t.Cleanup(func() { launcherPort = previous })
	return r, f, pod, fake
}

func TestLauncherClientAuthenticatedRoundTrip(t *testing.T) {
	r, f, pod, fake := launcherClientSetup(t)
	before := time.Now()
	state, err := r.callLauncher(launcherTestContext(t), f, pod, "op-1", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if state != fake.state {
		t.Fatalf("state %+v", state)
	}
	req := fake.lastReq
	if req.Operation != "op-1" || req.Generation != "gen-1" || len(req.Nonce) != 64 || req.DeadlineMS <= req.NotAfterMS {
		t.Fatalf("request not bound to operation and generation: %+v", req)
	}
	expires := time.UnixMilli(req.NotAfterMS)
	if expires.Before(before) || expires.After(before.Add(3*time.Second+time.Second)) {
		t.Fatalf("stop request must expire within three seconds, got %v", expires.Sub(before))
	}
	// A shorter caller deadline tightens the request expiry rather than extending it.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := r.callLauncher(ctx, f, pod, "", "gen-1"); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(fake.lastReq.NotAfterMS).After(time.Now().Add(1100 * time.Millisecond)) {
		t.Fatal("request expiry exceeded the caller deadline")
	}
	if fake.calls.Load() != 2 {
		t.Fatalf("calls %d", fake.calls.Load())
	}
}

func TestLauncherClientRejectsForgedOrMisboundResponses(t *testing.T) {
	cases := map[string]func(*fakeLauncher){
		"wrong response MAC":    func(l *fakeLauncher) { l.mutate = func(r *launcher.Response) { r.MAC = strings.Repeat("0", 64) } },
		"replayed nonce":        func(l *fakeLauncher) { l.mutate = func(r *launcher.Response) { r.Nonce = strings.Repeat("1", 64) } },
		"unsigned state change": func(l *fakeLauncher) { l.mutate = func(r *launcher.Response) { r.State.Phase = "Stopped" } },
		"other pod UID":         func(l *fakeLauncher) { l.state.PodUID = "someone-else" },
		"other pod name":        func(l *fakeLauncher) { l.state.Node = "alpha-1" },
		"other host":            func(l *fakeLauncher) { l.state.Host = "host-9" },
		"empty invocation":      func(l *fakeLauncher) { l.state.Invocation = "" },
		"wrong generation":      func(l *fakeLauncher) { l.state.Generation = "other" },
		"wrong operation":       func(l *fakeLauncher) { l.state.Operation = "other" },
		"empty generation":      func(l *fakeLauncher) { l.state.Generation = "" },
		"HTTP conflict":         func(l *fakeLauncher) { l.status = http.StatusConflict },
		"redirect":              func(l *fakeLauncher) { l.redirect = true },
		"oversized body":        func(l *fakeLauncher) { l.body = []byte(strings.Repeat(" ", 8193)) },
		"malformed body":        func(l *fakeLauncher) { l.body = []byte("{not json") },
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			r, f, pod, fake := launcherClientSetup(t)
			fault(fake)
			state, err := r.callLauncher(launcherTestContext(t), f, pod, "op-1", "gen-1")
			if err == nil {
				t.Fatalf("accepted %s: %+v", name, state)
			}
			if state != (launcher.State{}) {
				t.Fatalf("rejected response leaked state: %+v", state)
			}
		})
	}
}

func TestLauncherClientRefusesWithoutCredentialAuthority(t *testing.T) {
	for name, fault := range map[string]func(*testing.T, *Reconciler, *fleet.CelldFleet, *corev1.Pod){
		"key digest changed on reservation": func(t *testing.T, r *Reconciler, f *fleet.CelldFleet, _ *corev1.Pod) {
			res := &fleet.CelldStorageReservation{}
			if err := r.Get(t.Context(), client.ObjectKey{Name: reservationName(f)}, res); err != nil {
				t.Fatal(err)
			}
			res.Annotations[launcherKeyDigest] = "other"
			if err := r.Update(t.Context(), res); err != nil {
				t.Fatal(err)
			}
		},
		"secret relabeled to another fleet": func(t *testing.T, r *Reconciler, f *fleet.CelldFleet, _ *corev1.Pod) {
			s := &corev1.Secret{}
			if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: launcherSecretName(f)}, s); err != nil {
				t.Fatal(err)
			}
			s.Labels[FleetLabel] = "other"
			if err := r.Update(t.Context(), s); err != nil {
				t.Fatal(err)
			}
		},
		"pod without IP": func(_ *testing.T, _ *Reconciler, _ *fleet.CelldFleet, pod *corev1.Pod) { pod.Status.PodIP = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r, f, pod, fake := launcherClientSetup(t)
			fault(t, r, f, pod)
			if _, err := r.callLauncher(launcherTestContext(t), f, pod, "op-1", "gen-1"); err == nil {
				t.Fatal("call succeeded without credential authority")
			}
			if fake.calls.Load() != 0 {
				t.Fatal("controller contacted the launcher before verifying its own credential")
			}
		})
	}
}

func TestLauncherClientWrongKeyIsRejectedByLauncher(t *testing.T) {
	r, f, pod, fake := launcherClientSetup(t)
	s := &corev1.Secret{}
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: f.Namespace, Name: launcherSecretName(f)}, s); err != nil {
		t.Fatal(err)
	}
	// Both the Secret and its recorded digest agree, but the launcher was started
	// with a different key: its request MAC check must fail closed at the launcher.
	fake.key = []byte(strings.Repeat("z", 32))
	if _, err := r.callLauncher(launcherTestContext(t), f, pod, "op-1", "gen-1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected launcher 403 on request MAC mismatch, got %v", err)
	}
}

func launcherTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	return withRemovalDeadline(ctx, deadline)
}

func TestLauncherClientRequiresEveryIndependentCompletionProof(t *testing.T) {
	for _, fault := range []string{"none", "data-safe", "phase", "operation", "generation", "mode", "blocker", "control-only", "child", "lock", "restart"} {
		t.Run(fault, func(t *testing.T) {
			r, f, pod, fake := launcherClientSetup(t)
			completeLauncherRemoval(&fake.state, "op-1")
			switch fault {
			case "data-safe":
				fake.state.Removal.DataSafe = false
			case "phase":
				fake.state.Removal.Phase = "failed"
			case "operation":
				fake.state.Removal.Operation = "other"
			case "generation":
				fake.state.Removal.Generation = "other"
			case "mode":
				fake.state.Removal.Mode = "preserve"
			case "blocker":
				fake.state.Removal.Blocker = "blocked"
			case "control-only":
				fake.state.Removal.ControlOnly = false
			case "child":
				fake.state.ChildExited = false
			case "lock":
				fake.state.InheritedLockReleased = false
			case "restart":
				fake.state.RestartDenied = false
			}
			state, err := r.callLauncher(launcherTestContext(t), f, pod, "op-1", "gen-1")
			if (err == nil) != (fault == "none") {
				t.Fatalf("proof %s: %+v %v", fault, state, err)
			}
		})
	}
}

func TestLauncherMutationRequiresDeadlineButObservationDoesNot(t *testing.T) {
	r, f, pod, fake := launcherClientSetup(t)
	if _, err := r.callLauncher(t.Context(), f, pod, "op-1", "gen-1"); err == nil {
		t.Fatal("operation admitted without deadline")
	}
	if fake.calls.Load() != 0 {
		t.Fatal("sent unbounded operation")
	}
	if _, err := r.callLauncher(t.Context(), f, pod, "", ""); err != nil {
		t.Fatalf("observation requires no operation deadline: %v", err)
	}
}

func TestLauncherKeepsFixedOperationDeadlineUnderShortReconcileContext(t *testing.T) {
	r, f, pod, fake := launcherClientSetup(t)
	deadline := time.Now().Add(30 * time.Minute).Truncate(time.Millisecond)
	for _, timeout := range []time.Duration{time.Second, 2 * time.Second} {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		_, err := r.callLauncher(withRemovalDeadline(ctx, deadline), f, pod, "op-1", "gen-1")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if fake.lastReq.DeadlineMS != deadline.UnixMilli() {
			t.Fatal("transport context changed fixed operation deadline")
		}
		if fake.lastReq.NotAfterMS >= deadline.UnixMilli() {
			t.Fatal("request replay bound lost")
		}
	}
}
