// Command celld-launcher holds the retained-volume lock around unmodified celld.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	return launcher.Run(ctx, launcher.Config{Root: "/work", Address: ":8083", PodUID: os.Getenv("POD_UID"), Node: os.Getenv("CELLD_NODE"), Host: os.Getenv("NODE_NAME"), Key: key, Command: []string{"/usr/local/bin/celld"}, Spacing: 10 * time.Second, Stdout: os.Stdout, Stderr: os.Stderr})
}
