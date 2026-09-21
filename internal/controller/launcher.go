package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/launcher"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const launcherGate = "celld.eric.dev/exclusive-volume"

// launcherPort is the private launcher listener; tests point it at a local fake.
var launcherPort = "8083"

// A reconcile/HTTP context may have a shorter deadline than the operation.
// Preserve the recorded protocol deadline separately from request cancellation.
type removalDeadlineKey struct{}

func withRemovalDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, removalDeadlineKey{}, deadline)
}

const launcherKeyDigest = "celld.eric.dev/launcher-key-digest"

func launcherSecretName(f *fleet.CelldFleet) string { return f.Name + "-launcher" }
func persistentAccessModes(opts Options) []corev1.PersistentVolumeAccessMode {
	if opts.LauncherImage != "" && (!opts.LocalTest || opts.LocalRWOP) {
		return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	}
	return []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
}
func (r *Reconciler) createLauncherKey(ctx context.Context, f *fleet.CelldFleet, res *fleet.CelldStorageReservation) error {
	if !r.Options.LocalTest && !strings.Contains(r.Options.LauncherImage, "@sha256:") {
		return errors.New("launcher image must be digest pinned")
	}
	if res.Annotations[launcherKeyDigest] != "" {
		_, err := r.launcherKey(ctx, f)
		return err
	}
	const creationKey = "celld.eric.dev/launcher-creation"
	if res.Annotations == nil {
		res.Annotations = map[string]string{}
	}
	if res.Annotations[creationKey] == "" {
		res.Annotations[creationKey] = launcher.Nonce()
		if err := r.Update(ctx, res); err != nil {
			return err
		}
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: launcherSecretName(f)}, secret)
	if apierrors.IsNotFound(err) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		secret = &corev1.Secret{ObjectMeta: metadata(f, launcherSecretName(f)), Immutable: new(true), Data: map[string][]byte{"key": key}}
		secret.Annotations = map[string]string{creationKey: res.Annotations[creationKey]}
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("exclusive launcher credential creation: %w", err)
		}
	} else if err != nil {
		return err
	}
	key := secret.Data["key"]
	if secret.Annotations[creationKey] != res.Annotations[creationKey] || secret.Labels[FleetLabel] != string(f.UID) || len(key) != 32 || secret.Immutable == nil || !*secret.Immutable || !secret.DeletionTimestamp.IsZero() {
		return errors.New("launcher credential has no matching durable creation authority")
	}
	res.Annotations[launcherKeyDigest] = digest(key)
	return r.Update(ctx, res)
}
func (r *Reconciler) launcherKey(ctx context.Context, f *fleet.CelldFleet) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: launcherSecretName(f)}, secret); err != nil {
		return nil, err
	}
	res := &fleet.CelldStorageReservation{}
	if err := r.Get(ctx, client.ObjectKey{Name: reservationName(f)}, res); err != nil {
		return nil, err
	}
	key := secret.Data["key"]
	if len(key) != 32 || secret.Labels[FleetLabel] != string(f.UID) || !secret.DeletionTimestamp.IsZero() || secret.Immutable == nil || !*secret.Immutable || res.Annotations[launcherKeyDigest] != digest(key) {
		return nil, errors.New("launcher credential identity changed")
	}
	return key, nil
}
func (r *Reconciler) callLauncher(ctx context.Context, f *fleet.CelldFleet, pod *corev1.Pod, operation, generation string) (launcher.State, error) {
	if r.launcherCall != nil {
		return r.launcherCall(ctx, f, pod, operation, generation)
	}
	var zero launcher.State
	if net.ParseIP(pod.Status.PodIP) == nil {
		return zero, errors.New("launcher Pod IP unavailable")
	}
	key, err := r.launcherKey(ctx, f)
	if err != nil {
		return zero, err
	}
	expires := time.Now().Add(3 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
		expires = deadline
	}
	req := launcher.Request{Nonce: launcher.Nonce(), Operation: operation, Generation: generation, NotAfterMS: expires.UnixMilli()}
	if operation != "" {
		deadline, ok := ctx.Value(removalDeadlineKey{}).(time.Time)
		if !ok || deadline.IsZero() {
			return zero, errors.New("strict launcher stop requires an operation deadline")
		}
		req.DeadlineMS = deadline.UnixMilli()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(pod.Status.PodIP, launcherPort)+"/v2", bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	request.Header.Set("X-Celld-MAC", launcher.MAC(key, "request", req))
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	c := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("launcher redirects forbidden") }}
	response, err := c.Do(request)
	if err != nil {
		return zero, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("launcher HTTP %d", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(b) > 8192 {
		return zero, errors.New("launcher response incomplete or oversized")
	}
	var answer launcher.Response
	if err := json.Unmarshal(b, &answer); err != nil {
		return zero, err
	}
	signed := struct {
		Nonce string
		State launcher.State
	}{answer.Nonce, answer.State}
	if answer.Nonce != req.Nonce || !launcher.Verify(key, "response", signed, answer.MAC) || answer.State.PodUID != string(pod.UID) || answer.State.Node != runtimeNode(f, pod) || answer.State.Host != pod.Spec.NodeName || answer.State.Invocation == "" || answer.State.Generation == "" {
		return zero, errors.New("launcher association or authentication failed")
	}
	if (generation != "" && answer.State.Generation != generation) || (operation != "" && answer.State.Operation != operation) {
		return zero, errors.New("launcher response operation or generation changed")
	}
	if answer.State.Phase == "Stopped" && !answer.State.RemovalReady() {
		return zero, errors.New("launcher completion lacks strict runtime or process proof")
	}
	return answer.State, nil
}
func healthyHost(node *corev1.Node) bool {
	return node.UID != "" && node.Status.NodeInfo.BootID != "" && node.DeletionTimestamp.IsZero() && slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
	})
}
