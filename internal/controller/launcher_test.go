package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These opt-in checks execute the actual unchanged runtime image, without network
// or credentials. They prove bounded launcher behavior, not Kubernetes fencing.
func TestLauncher(t *testing.T) {
	if os.Getenv("CELLD_DOCKER_TEST") != "1" {
		t.Skip("set CELLD_DOCKER_TEST=1 for isolated real-image launcher tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	t.Run("full-ttl-before-exec", func(t *testing.T) {
		started := time.Now()
		out := docker("run", "--rm", "--network=none", "--entrypoint=/bin/sh", Image, "-c", launch+" --version")
		if time.Since(started) < 10*time.Second || !strings.Contains(out, "0.5.0") {
			t.Fatalf("incorrect launch spacing or image version: %s", out)
		}
	})
	t.Run("termination-before-exec", func(t *testing.T) {
		name := fmt.Sprintf("celld-launch-test-%d", time.Now().UnixNano())
		docker("run", "-d", "--name", name, "--network=none", "--entrypoint=/bin/sh", Image, "-c", launch+" --version")
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cleanupCancel()
			if out, err := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
				t.Errorf("cleanup: %v: %s", err, out)
			}
		})
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
		docker("kill", "--signal=TERM", name)
		if code := strings.TrimSpace(docker("wait", name)); code != "0" {
			t.Fatalf("launcher did not handle TERM: %s", code)
		}
		if out := strings.TrimSpace(docker("logs", name)); out != "" {
			t.Fatalf("runtime started after TERM during delay: %s", out)
		}
	})
}
