package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ewhauser/celld-operator/internal/launcher"
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Opt-in local qualification: real runtime/launcher, simulated Kubernetes and CSI.
func TestStrictRuntimeCurrentOperation(t *testing.T) {
	binary := os.Getenv("CELLD_STRICT_TEST_BINARY")
	if binary == "" {
		t.Skip("set CELLD_STRICT_TEST_BINARY to a built strict-shutdown celld; requires Docker")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	t.Logf("strict celld binary SHA256 %s", hex.EncodeToString(hash[:]))
	name := "launcher-strict-" + launcher.Nonce()[:12]
	image := "quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"
	// Isolate this disposable object-store fixture from Docker disk pressure.
	// This handshake does not test object-store restart durability.
	cmd := exec.CommandContext(t.Context(), "docker", "run", "-d", "--tmpfs", "/data:rw,size=1g", "--name", name, "-p", "127.0.0.1::9000", "-e", "MINIO_ROOT_USER=launcher", "-e", "MINIO_ROOT_PASSWORD=launcher-local-only", image, "server", "/data")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("MinIO: %s %v", out, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("MinIO cleanup: %s %v", out, err)
		}
	})
	port, err := exec.CommandContext(t.Context(), "docker", "port", name, "9000/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + strings.TrimSpace(string(port))
	bucket := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: &endpoint, UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("launcher", "launcher-local-only", ""), RetryMaxAttempts: 1})
	until := time.Now().Add(30 * time.Second)
	for {
		_, err = bucket.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("launcher")})
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	x := newOperationFixture(t, "PersistentFleet")
	pod := &corev1.Pod{}
	if err := x.r.Get(t.Context(), client.ObjectKey{Namespace: x.f.Namespace, Name: "alpha-2"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.PodIP = "127.0.0.1"
	if err := x.r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	key, err := x.r.launcherKey(t.Context(), x.f)
	if err != nil {
		t.Fatal(err)
	}
	address := strictTestAddress(t)
	_, portNumber, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	oldPort := launcherPort
	launcherPort = portNumber
	t.Cleanup(func() { launcherPort = oldPort })
	c := launcher.Config{Root: t.TempDir(), Address: address, PodUID: string(pod.UID), Node: pod.Name, Host: pod.Spec.NodeName, BootID: "boot", Key: key, StopGrace: time.Second}

	c.Command = []string{binary}
	log, err := os.CreateTemp(t.TempDir(), "celld-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	c.Stdout, c.Stderr = log, log
	internal := strictTestAddress(t)
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "launcher", "AWS_SECRET_ACCESS_KEY": "launcher-local-only", "AWS_REGION": "us-east-1", "AWS_ALLOW_HTTP": "true", "S3_ENDPOINT": endpoint,
		"CELLD_BUCKET": "s3://launcher", "CELLD_NODE": c.Node, "CELLD_WATCH": c.Root,
		"CELLD_ADDR": strictTestAddress(t), "CELLD_INTERNAL_ADDR": internal, "CELLD_ADVERTISE": internal,
		"CELLD_UNSAFE_PUBLIC_ADVERTISE": "1", "CELLD_DURABILITY": "fleet", "CELLD_TOKIO_THREADS": "2", "CELLD_SHUTDOWN_TOTAL_MS": "10000", "CELLD_REBALANCE_INTERVAL_MS": "0",
	} {
		t.Setenv(key, value)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "wrangler.jsonc"), []byte(`{"name":"launcher-test","main":"index.js","compatibility_date":"2026-01-01"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "index.js"), []byte(`export default {fetch() {return new Response("ok")}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if esbuild := os.Getenv("CELLD_STRICT_TEST_ESBUILD"); esbuild != "" {
		t.Setenv("CELLD_ESBUILD", esbuild)
	}
	deployCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(deployCtx, binary, "deploy", project).CombinedOutput(); err != nil {
		t.Fatalf("deploy: %s %v", out, err)
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", internal)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	c.Control = controlplane.New(transport)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	exited := make(chan error, 1)
	go func() { exited <- launcher.Run(ctx, c) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-exited:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("launcher failed to exit")
		}
	})
	x.r.launcherCall = nil
	until = time.Now().Add(30 * time.Second)
	for {
		state, err := x.r.callLauncher(t.Context(), x.f, pod, "", "")
		if err == nil && state.Phase == "Running" {
			snapshot, err := c.Control.State(t.Context(), controlplane.Target{IP: "127.0.0.1", Node: pod.Name, Generation: state.Generation})
			if err == nil && snapshot.Capabilities.SupportsRemoveDisk() {
				break
			}
		}
		if time.Now().After(until) {
			logs, _ := os.ReadFile(log.Name())
			t.Fatalf("runtime startup timed out: %v\n%s", err, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	x.desired(2)
	until = time.Now().Add(time.Minute)
	for {
		x.step()
		s := x.state()
		if s.Operation != nil && s.Operation.Phase == "ProofCaptured" {
			break
		}
		if time.Now().After(until) {
			logs, _ := os.ReadFile(log.Name())
			t.Fatalf("controller failed to persist runtime proof: %+v\n%s", s.Operation, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	proof := x.state().Operation.Targets[0].Proof
	if proof == nil || !validProof(x.state().Operation, x.state().Operation.Targets[0], *proof) {
		t.Fatal("controller omitted strict launcher proof")
	}
	// Lose all volatile launcher results after Kubernetes capture. A fresh
	// controller must issue only the recorded workload effect from durable proof.
	stop()
	x.r = &Reconciler{Client: x.r.Client, Options: x.r.Options, NetworkPolicyEnforced: true, Collector: x.r.Collector, now: x.r.now}
	x.finish()
	if x.state().Applied != 2 || x.state().Operation != nil {
		t.Fatal("strict operation did not finish after CSI cleanup")
	}
	t.Log("real fork runtime + launcher HTTP -> persisted exact proof -> controller restart -> guarded replica/PVC removal -> simulated CSI completion; Kubernetes/CSI are fixtures")
}
func strictTestAddress(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
