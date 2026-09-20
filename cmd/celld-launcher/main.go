// Command celld-launcher holds the retained-volume lock around unmodified celld.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "install" {
		b, e := os.ReadFile("/celld-launcher")
		if e != nil {
			return e
		}
		if e := os.MkdirAll(filepath.Dir(os.Args[2]), 0o755); e != nil {
			return e
		}
		return os.WriteFile(os.Args[2], b, 0o755)
	}
	key, e := os.ReadFile("/launcher-key/key")
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// The pod's own terminationGracePeriodSeconds. The launcher derives both
	// bounds of an unrequested termination from it, so their sum fits inside the
	// window kubelet allows. Unset means the default 30 second grace.
	var grace time.Duration
	if raw := os.Getenv("LAUNCHER_TERMINATION_GRACE_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 {
			return fmt.Errorf("invalid LAUNCHER_TERMINATION_GRACE_SECONDS %q", raw)
		}
		grace = time.Duration(seconds) * time.Second
	} else if raw := os.Getenv("LAUNCHER_STOP_GRACE_SECONDS"); raw != "" {
		// Pods templated before the budget was derived carry the pod grace minus
		// the same five second margin. Templates are never rolled out, so such a
		// pod can restart onto this binary: recover the grace it was told about
		// rather than silently falling back to the default.
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 {
			return fmt.Errorf("invalid LAUNCHER_STOP_GRACE_SECONDS %q", raw)
		}
		grace = time.Duration(seconds+5) * time.Second
	}
	return launcher.Run(ctx, launcher.Config{Root: "/work", Address: ":8083", PodUID: os.Getenv("POD_UID"), Node: os.Getenv("CELLD_NODE"), Host: os.Getenv("NODE_NAME"), Key: key, Command: []string{"/usr/local/bin/celld"}, Spacing: 10 * time.Second, Grace: grace, Stdout: os.Stdout, Stderr: os.Stderr})
}
