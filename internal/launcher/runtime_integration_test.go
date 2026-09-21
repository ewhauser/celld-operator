package launcher

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
	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
)

// This opt-in test runs an actual strict celld binary and isolated Docker MinIO.
// It qualifies the supervisor/HTTP/identity handshake on an empty disk, not
// replication recovery, EKS or EBS. Ordinary suites use explicit HTTP fixtures.
func TestStrictRuntimeSupervisorHTTP(t *testing.T) {
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
	name := "launcher-strict-" + Nonce()[:12]
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
	c := config(t)
	c.Command = []string{binary}
	log, err := os.CreateTemp(t.TempDir(), "celld-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	c.Stdout, c.Stderr = log, log
	internal := testAddress(t)
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "launcher", "AWS_SECRET_ACCESS_KEY": "launcher-local-only", "AWS_REGION": "us-east-1", "AWS_ALLOW_HTTP": "true", "S3_ENDPOINT": endpoint,
		"CELLD_BUCKET": "s3://launcher", "CELLD_NODE": c.Node, "CELLD_WATCH": c.Root,
		"CELLD_ADDR": testAddress(t), "CELLD_INTERNAL_ADDR": internal, "CELLD_ADVERTISE": internal,
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
	startSupervisor(t, c)
	running := awaitPhase(t, c.Address, c.Key, "Running")
	target := controlplane.Target{IP: "127.0.0.1", Node: c.Node, Generation: running.Generation}
	until = time.Now().Add(30 * time.Second)
	for {
		state, err := c.Control.State(t.Context(), target)
		if err == nil && state.Capabilities.SupportsRemoveDisk() {
			break
		}
		if time.Now().After(until) {
			body, _ := os.ReadFile(log.Name())
			t.Fatalf("strict runtime startup: %v\n%s", err, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := query(t, c.Address, c.Key, "live-removal", running.Generation); err != nil {
		t.Fatal(err)
	}
	stopped := awaitPhase(t, c.Address, c.Key, "Stopped")
	if !stopped.RemovalReady() {
		t.Fatalf("missing exact proof: %+v", stopped)
	}
	t.Logf("captured strict control-only result, exact child exit, inherited-lock release and durable restart denial: %+v", stopped)
}
