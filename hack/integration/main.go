// Command integration runs the disposable kind + Calico + MinIO suites. It
// never reads the default kubeconfig. All created infrastructure is local and
// removed on exit, even on assertion failure or interruption.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type options struct {
	suite, runtimeImage, upgradeImage, operatorImage string
}

type harness struct {
	ctx                         context.Context //nolint:containedctx // one interrupt context spans the disposable run
	opts                        options
	root, name, tmp, kubeconfig string
	env                         []string
	nodes                       []string
	arch, launcherImage         string
	operatorArgs                []string
	created, builtLauncher      bool
}

func main() {
	os.Exit(realMain())
}

func realMain() (code int) {
	var opts options
	fs := flag.NewFlagSet("integration", flag.ContinueOnError)
	fs.StringVar(&opts.suite, "suite", "all", "suite: all, lifecycle, maintenance, faults, external")
	fs.StringVar(&opts.runtimeImage, "runtime-image", os.Getenv("CELLD_RUNTIME_IMAGE"), "required immutable ghcr.io/ewhauser/celld@sha256:... fork image")
	fs.StringVar(&opts.upgradeImage, "upgrade-image", os.Getenv("CELLD_UPGRADE_IMAGE"), "optional second fork digest for live upgrade qualification")
	fs.StringVar(&opts.operatorImage, "operator-image", "", "published controller/launcher image to qualify instead of building source")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments:", fs.Args())
		return 2
	}
	if !runtimeImagePin.MatchString(opts.runtimeImage) {
		fmt.Fprintln(os.Stderr, "--runtime-image or CELLD_RUNTIME_IMAGE must name the strict fork by immutable digest")
		return 2
	}
	if opts.upgradeImage != "" && (!runtimeImagePin.MatchString(opts.upgradeImage) || opts.upgradeImage == opts.runtimeImage) {
		fmt.Fprintln(os.Stderr, "--upgrade-image must name a different immutable strict fork digest")
		return 2
	}
	switch opts.suite {
	case "all", "lifecycle", "maintenance", "faults", "external":
	default:
		fmt.Fprintln(os.Stderr, "unknown suite:", opts.suite)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	h := &harness{ctx: ctx, opts: opts, root: root, name: "celld-strict-" + hex.EncodeToString(suffix)}
	h.tmp, err = os.MkdirTemp("", h.name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(h.tmp) }()
	h.kubeconfig = filepath.Join(h.tmp, "kubeconfig")
	h.env = append(append([]string{}, os.Environ()...), "KUBECONFIG="+h.kubeconfig)
	h.nodes = []string{h.name + "-control-plane", h.name + "-worker", h.name + "-worker2"}
	defer func() {
		if err := h.cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			h.diagnostics(recovered)
			code = 1
		}
	}()
	h.exercise()
	return 0
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the celld-operator module")
		}
		dir = parent
	}
}

func (h *harness) exercise() {
	h.createCluster()
	h.loadImages()
	h.deployStore()
	h.deployApplication()
	h.installStorageClass()
	h.startOperator()
	h.exerciseIsolation()
	switch h.opts.suite {
	case "all":
		h.exerciseLifecycle()
		h.exerciseExternal()
		h.exerciseFaults()
		h.exerciseMaintenance()
	case "lifecycle":
		h.exerciseLifecycle()
	case "maintenance":
		h.exerciseMaintenance()
	case "faults":
		h.exerciseFaults()
	case "external":
		h.exerciseExternal()
	}
	fmt.Println("PASS: strict control-plane integration suite", h.opts.suite, "runtime", h.opts.runtimeImage)
}

// diagnostics dumps cluster state after a failure, then optionally holds the
// explicitly owned cluster open for inspection. Interrupted runs never hold.
func (h *harness) diagnostics(recovered any) {
	if f, ok := recovered.(*failure); ok {
		fmt.Fprintf(os.Stderr, "FAIL: %s\n%s", f.msg, f.stack)
	} else {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", recovered)
	}
	if _, err := os.Stat(h.kubeconfig); err == nil {
		for _, args := range [][]string{
			{"get", "pods", "-A", "-o", "wide"},
			{"-n", "fleets", "get", "celldfleets", "-o", "yaml"},
			{"get", "celldstoragereservations", "-o", "json"},
			{"-n", "fleets", "get", "events", "--sort-by=.lastTimestamp"},
			// An autoscaler that declines to act leaves nothing in the objects
			// above: it records why in its own conditions, and the answer is
			// usually what it measured rather than what the operator allowed.
			{"-n", "fleets", "describe", "hpa"},
			{"top", "pods", "-A"},
			{"get", "pv", "-o", "yaml"},
			{"-n", "fleets", "get", "pvc", "-o", "wide"},
			{"get", "volumeattachments", "-o", "yaml"},
			{"-n", "fleets", "logs", "alpha-0", "--all-containers=true", "--tail=80"},
			{"-n", "fleets", "logs", "beta-0", "--all-containers=true", "--tail=80"},
		} {
			out, err := h.try(command{args: h.kubectl(args...), timeout: 5 * time.Minute, background: true})
			if err != nil {
				fmt.Println(err)
				continue
			}
			fmt.Println(out)
		}
	}
	if out, err := h.try(command{args: h.kubectl("-n", "celld-system", "logs", "deployment/celld-operator"), timeout: 5 * time.Minute, background: true}); err == nil {
		fmt.Println(out)
	}
	hold, _ := strconv.Atoi(os.Getenv("CELLD_TEST_DIAGNOSTIC_HOLD_SECONDS"))
	hold = min(600, max(0, hold))
	if hold == 0 || h.ctx.Err() != nil {
		return
	}
	marker := filepath.Join(h.tmp, "continue-cleanup")
	fmt.Println("Diagnostic hold:", h.kubeconfig, "context=kind-"+h.name, "touch", marker, "to clean early")
	until := time.Now().Add(time.Duration(hold) * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// cleanup deletes only this invocation's cluster
// and launcher image, using a fresh context so an interrupt cannot skip it.
func (h *harness) cleanup() error {
	var errs []error
	if h.created {
		fmt.Println("Cleaning up only cluster", h.name)
		if _, err := h.try(command{args: []string{"kind", "delete", "cluster", "--name", h.name, "--kubeconfig", h.kubeconfig}, timeout: 3 * time.Minute, background: true}); err != nil {
			errs = append(errs, err)
		}
	}
	if h.builtLauncher {
		if _, err := h.try(command{args: []string{"docker", "image", "rm", h.launcherImage}, timeout: time.Minute, background: true}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
