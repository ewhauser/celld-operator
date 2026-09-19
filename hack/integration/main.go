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
	bucketLifecycle     bool
	orderedBucket       bool
	persistentLifecycle bool
	maintenance         bool
	faults              bool
	rwopCSI             bool
	external            bool
}

type harness struct {
	ctx  context.Context //nolint:containedctx // one interrupt context spans the whole disposable run
	opts options
	// persistentLifecycle and bucketLifecycle are the derived suite groups.
	persistentLifecycle bool
	bucketLifecycle     bool
	root                string
	name                string
	tmp                 string
	kubeconfig          string
	env                 []string
	nodes               []string
	arch                string
	operatorKubeconfig  string
	operatorLog         *os.File
	operatorArgs        []string
	process             *process
	created             bool
	launcherImage       string
}

func main() {
	os.Exit(realMain())
}

func realMain() (code int) {
	var opts options
	fs := flag.NewFlagSet("integration", flag.ContinueOnError)
	fs.BoolVar(&opts.bucketLifecycle, "bucket-lifecycle", false, "in-cluster manager with Metrics Server: Bucket shrink/grow and automatic contraction")
	fs.BoolVar(&opts.orderedBucket, "ordered-bucket", false, "Ordered Bucket placement, membership and ledger across two zones")
	fs.BoolVar(&opts.persistentLifecycle, "persistent-lifecycle", false, "launcher-managed PersistentFleet growth, graceful retirement and same-host reactivation")
	fs.BoolVar(&opts.maintenance, "maintenance", false, "live same-pin restart and RetainData deletion")
	fs.BoolVar(&opts.faults, "faults", false, "fault injection: manager crash points, node loss, S3 latency/partition via toxiproxy")
	fs.BoolVar(&opts.rwopCSI, "rwop-csi", false, "serve PersistentFleet claims as ReadWriteOncePod through the per-node hostpath CSI driver instead of local-path RWO")
	fs.BoolVar(&opts.external, "external", false, "External capacity mode: a HorizontalPodAutoscaler drives spec.replicas through the /scale subresource")
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
	h := &harness{
		ctx:                 ctx,
		opts:                opts,
		persistentLifecycle: opts.persistentLifecycle || opts.maintenance || opts.faults,
		root:                root,
		name:                "celld-step2-" + hex.EncodeToString(suffix),
	}
	h.bucketLifecycle = opts.bucketLifecycle || h.persistentLifecycle || opts.orderedBucket || opts.external
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
	if h.opts.orderedBucket {
		h.exerciseOrderedBucket()
		return
	}
	h.exerciseIsolation()
	switch {
	case h.opts.maintenance:
		h.exerciseMaintenance()
	case h.opts.faults:
		h.exerciseFaults()
	case h.opts.external:
		h.exerciseExternal()
	case h.persistentLifecycle:
		h.exercisePersistentLifecycle()
	default:
		h.exerciseAdditiveCapacity()
		if h.bucketLifecycle {
			h.exerciseBucketLifecycle()
			return
		}
		h.exerciseBaseRemainder()
	}
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
		} {
			out, err := h.try(command{args: h.kubectl(args...), timeout: 5 * time.Minute, background: true})
			if err != nil {
				fmt.Println(err)
				continue
			}
			fmt.Println(out)
		}
	}
	if logText, err := os.ReadFile(filepath.Join(h.tmp, "operator.log")); err == nil {
		fmt.Println(string(logText))
	}
	if h.bucketLifecycle {
		if out, err := h.try(command{args: h.kubectl("-n", "celld-system", "logs", "deployment/celld-operator"), timeout: 5 * time.Minute, background: true}); err == nil {
			fmt.Println(out)
		}
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

// cleanup stops the native manager and deletes only this invocation's cluster
// and launcher image, using a fresh context so an interrupt cannot skip it.
func (h *harness) cleanup() error {
	if h.process != nil {
		h.process.stop(true)
	}
	if h.operatorLog != nil {
		_ = h.operatorLog.Close()
	}
	var errs []error
	if h.created {
		fmt.Println("Cleaning up only cluster", h.name)
		if _, err := h.try(command{args: []string{"kind", "delete", "cluster", "--name", h.name, "--kubeconfig", h.kubeconfig}, timeout: 3 * time.Minute, background: true}); err != nil {
			errs = append(errs, err)
		}
	}
	if h.launcherImage != "" {
		if _, err := h.try(command{args: []string{"docker", "image", "rm", h.launcherImage}, timeout: time.Minute, background: true}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
