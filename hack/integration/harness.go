package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// object is a decoded Kubernetes JSON document.
type object = map[string]any

// failure aborts the run. main recovers it, prints diagnostics, and cleans up.
type failure struct {
	msg   string
	stack []byte
}

func fail(format string, args ...any) {
	panic(&failure{msg: fmt.Sprintf(format, args...), stack: debug.Stack()})
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func assert(condition bool, format string, args ...any) {
	if !condition {
		fail("assertion failed: "+format, args...)
	}
}

// command is one bounded child process. Output is stdout and stderr combined.
type command struct {
	args    []string
	timeout time.Duration
	dir     string
	env     []string
	stdin   string
	// background commands run under a fresh context so cleanup survives interrupts.
	background bool
}

func (h *harness) try(c command) (string, error) {
	base := h.ctx
	if c.background {
		base = context.Background()
	} else if h.ctx.Err() != nil {
		fail("integration interrupted")
	}
	ctx, cancel := context.WithTimeout(base, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.args[0], c.args[1:]...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	if cmd.Env == nil {
		cmd.Env = h.env
	}
	if c.stdin != "" {
		cmd.Stdin = strings.NewReader(c.stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if !c.background && h.ctx.Err() != nil {
		fail("integration interrupted")
	}
	switch {
	case err == nil:
		return out.String(), nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return out.String(), fmt.Errorf("%v: timed out after %s: %s", c.args, c.timeout, out.String())
	default:
		return out.String(), fmt.Errorf("%v: %s", c.args, out.String())
	}
}

func (h *harness) run(c command) string {
	out, err := h.try(c)
	must(err)
	return out
}

func (h *harness) sh(timeout time.Duration, args ...string) {
	h.run(command{args: args, timeout: timeout})
}

// kubectl commands only ever address this invocation's cluster.
func (h *harness) kubectl(args ...string) []string {
	return append([]string{"kubectl", "--kubeconfig", h.kubeconfig, "--context", "kind-" + h.name}, args...)
}

func (h *harness) tryK(args ...string) (string, error) {
	return h.try(command{args: h.kubectl(args...), timeout: 5 * time.Minute})
}

func (h *harness) k(args ...string) string {
	out, err := h.tryK(args...)
	must(err)
	return out
}

func (h *harness) tryApply(obj any) error {
	body, err := json.Marshal(obj)
	must(err)
	_, err = h.try(command{args: h.kubectl("apply", "-f", "-"), timeout: 5 * time.Minute, stdin: string(body)})
	return err
}

func (h *harness) apply(obj any) {
	must(h.tryApply(obj))
}

func (h *harness) applyText(manifest string) {
	h.run(command{args: h.kubectl("apply", "-f", "-"), timeout: 5 * time.Minute, stdin: manifest})
}

func decode(text string) object {
	var out object
	must(json.Unmarshal([]byte(text), &out))
	return out
}

func encode(v any) string {
	body, err := json.Marshal(v)
	must(err)
	return string(body)
}

func (h *harness) tryGet(ns, kind, name string) (object, error) {
	out, err := h.tryK("-n", ns, "get", kind, name, "-o", "json")
	if err != nil {
		return nil, err
	}
	return decode(out), nil
}

func (h *harness) getIn(ns, kind, name string) object {
	value, err := h.tryGet(ns, kind, name)
	must(err)
	return value
}

func (h *harness) get(kind, name string) object {
	return h.getIn("fleets", kind, name)
}

func (h *harness) cluster(kind, name string) object {
	return decode(h.k("get", kind, name, "-o", "json"))
}

func (h *harness) listIn(ns, kind string, extra ...string) []object {
	args := append([]string{"-n", ns, "get", kind}, extra...)
	return items(decode(h.k(append(args, "-o", "json")...)))
}

func (h *harness) reservations() []object {
	return items(decode(h.k("get", "celldstoragereservations", "-o", "json")))
}

func (h *harness) fleetPods(fleetName string) []object {
	return h.listIn("fleets", "pods", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", fleetName)))
}

// merge applies a JSON merge patch to a fleet in the fleets namespace.
func (h *harness) merge(name, patch string) {
	h.k("-n", "fleets", "patch", "celldfleet", name, "--type=merge", "-p", patch)
}

func (h *harness) tryMerge(kind, name, patch string) error {
	_, err := h.tryK("-n", "fleets", "patch", kind, name, "--type=merge", "-p", patch)
	return err
}

func (h *harness) setReplicas(fleetName string, replicas int) {
	h.merge(fleetName, fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
}

func (h *harness) succeeded(ns, pod string) bool {
	phase := str(h.getIn(ns, "pod", pod), "status", "phase")
	if phase == "Failed" {
		fail("Test Pod failed: %s\n%s", pod, h.k("-n", ns, "logs", pod))
	}
	return phase == "Succeeded"
}

const defaultWait = 180 * time.Second

func (h *harness) wait(description string, check func() bool) {
	h.waitFor(description, defaultWait, check)
}

func (h *harness) waitFor(description string, timeout time.Duration, check func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			fmt.Println("PASS:", description)
			return
		}
		h.sleep(2 * time.Second)
	}
	fail("Timed out: %s", description)
}

// hold asserts an invariant continuously for a bounded window.
func (h *harness) hold(window time.Duration, description string, invariant func() bool) {
	until := time.Now().Add(window)
	for time.Now().Before(until) {
		assert(invariant(), "violated during hold: %s", description)
		h.sleep(2 * time.Second)
	}
	fmt.Println("PASS:", description, "held for", int(window.Seconds()), "s")
}

func (h *harness) sleep(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-h.ctx.Done():
		fail("integration interrupted")
	}
}

func (h *harness) fetch(url string, timeout time.Duration) []byte {
	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	must(err)
	response, err := http.DefaultClient.Do(request)
	must(err)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		fail("GET %s: %s", url, response.Status)
	}
	body, err := io.ReadAll(response.Body)
	must(err)
	return body
}

// process is the native manager binary used outside the in-cluster suites.
type process struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (h *harness) startNativeOperator() {
	cmd := exec.CommandContext(h.ctx, h.root+"/bin/celld-operator", "--network-policy-enforced", "--local-test")
	cmd.Env = append(append([]string{}, os.Environ()...), "KUBECONFIG="+h.operatorKubeconfig)
	cmd.Stdout = h.operatorLog
	cmd.Stderr = h.operatorLog
	must(cmd.Start())
	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	h.process = p
}

func (p *process) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *process) waitFor(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// stop terminates the manager and waits 20 s; kill escalates instead of failing.
func (p *process) stop(kill bool) {
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	if p.waitFor(20 * time.Second) {
		return
	}
	if !kill {
		fail("native operator did not exit within 20 s of SIGTERM")
	}
	_ = p.cmd.Process.Kill()
	p.waitFor(10 * time.Second)
}

// JSON accessors. A missing or mistyped path yields the zero value, matching the
// permissive .get() chains of the original harness.
func field(o object, path ...string) any {
	var current any = o
	for _, key := range path {
		m, ok := current.(object)
		if !ok {
			return nil
		}
		current, ok = m[key]
		if !ok {
			return nil
		}
	}
	return current
}

func str(o object, path ...string) string {
	value, _ := field(o, path...).(string)
	return value
}

func num(o object, path ...string) int64 {
	switch value := field(o, path...).(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func boolean(o object, path ...string) bool {
	value, _ := field(o, path...).(bool)
	return value
}

func sub(o object, path ...string) object {
	value, _ := field(o, path...).(object)
	return value
}

func list(o object, path ...string) []object {
	raw, _ := field(o, path...).([]any)
	out := make([]object, 0, len(raw))
	for _, entry := range raw {
		if m, ok := entry.(object); ok {
			out = append(out, m)
		}
	}
	return out
}

func strs(o object, path ...string) []string {
	raw, _ := field(o, path...).([]any)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func items(o object) []object      { return list(o, "items") }
func uidOf(o object) string        { return str(o, "metadata", "uid") }
func nameOf(o object) string       { return str(o, "metadata", "name") }
func conditions(o object) []object { return list(o, "status", "conditions") }
func generation(o object) int64    { return num(o, "metadata", "generation") }
func specReplicas(o object) int64  { return num(o, "spec", "replicas") }
func operationID(o object) string  { return str(o, "status", "lifecycle", "operationID") }
func deleting(o object) bool       { return str(o, "metadata", "deletionTimestamp") != "" }
func annotation(o object, key string) string {
	return str(o, "metadata", "annotations", key)
}

func hasReason(o object, reasons ...string) bool {
	for _, c := range conditions(o) {
		for _, reason := range reasons {
			if str(c, "reason") == reason {
				return true
			}
		}
	}
	return false
}

func condition(o object, kind string) object {
	for _, c := range conditions(o) {
		if str(c, "type") == kind {
			return c
		}
	}
	return nil
}

func isReady(o object) bool {
	c := condition(o, "Ready")
	return c != nil && str(c, "status") == "True"
}

func same(a, b any) bool { return reflect.DeepEqual(a, b) }

func podUIDs(pods []object) map[string]bool {
	out := map[string]bool{}
	for _, pod := range pods {
		out[uidOf(pod)] = true
	}
	return out
}

func nodeNames(pods []object) map[string]bool {
	out := map[string]bool{}
	for _, pod := range pods {
		out[str(pod, "spec", "nodeName")] = true
	}
	return out
}
