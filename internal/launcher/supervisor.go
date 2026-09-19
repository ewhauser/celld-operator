package launcher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Root, Address, PodUID, Node, Host, BootID string
	Key                                       []byte
	Command                                   []string
	Spacing                                   time.Duration
	Stdout, Stderr                            io.Writer
}
type supervisor struct {
	mu       sync.Mutex
	state    State
	stop     chan struct{}
	stopping bool
	key      []byte
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/v1" {
		http.Error(w, "unsupported", 404)
		return
	}
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || len(req.Nonce) != 64 || !Verify(s.key, "request", req, r.Header.Get("X-Celld-MAC")) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Operation != "" {
		if (s.state.Phase == "Running" && req.NotAfterMS <= time.Now().UnixMilli()) || req.Generation != s.state.Generation || (s.state.Operation != "" && s.state.Operation != req.Operation) || (s.state.Phase != "Running" && s.state.Phase != "Stopping" && s.state.Phase != "Stopped") {
			http.Error(w, "invocation changed or unavailable", http.StatusConflict)
			return
		}
		s.state.Operation = req.Operation
		if !s.stopping {
			s.stopping = true
			s.state.Phase = "Stopping"
			close(s.stop)
		}
	}
	body := struct {
		Nonce string
		State State
	}{req.Nonce, s.state}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{Nonce: req.Nonce, State: s.state, MAC: MAC(s.key, "response", body)})
}
func (s *supervisor) phase(phase string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Phase = phase
	if err != nil {
		s.state.Error = err.Error()
	}
}
func openLock(root string) (*os.File, error) {
	f, e := os.OpenFile(filepath.Join(root, ".celld-launcher.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		return nil, e
	}
	return f, nil
}
func waitLock(ctx context.Context, root string) (*os.File, error) {
	for {
		f, e := openLock(root)
		if e == nil {
			return f, nil
		}
		if !errors.Is(e, syscall.EWOULDBLOCK) {
			return nil, e
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Run never reconstructs a stopped certificate from a disk marker. Only the
// live supervisor that waited for its exact child and reacquired the lock can
// answer Stopped. After a crash, a successor must acquire the inherited lock.
func Run(ctx context.Context, c Config) error {
	if len(c.Key) != 32 || c.PodUID == "" || c.Node == "" || c.Host == "" || len(c.Command) == 0 || c.Spacing < 0 {
		return errors.New("invalid launcher configuration")
	}
	if err := os.MkdirAll(c.Root, 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s := &supervisor{state: State{PodUID: c.PodUID, Node: c.Node, Host: c.Host, Invocation: Nonce(), Generation: hex.EncodeToString(pub), Phase: "WaitingForExclusiveVolume"}, stop: make(chan struct{}), key: c.Key}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second}
	defer func() { _ = server.Close() }()
	go func() { _ = server.Serve(listener) }()
	lock, err := waitLock(ctx, c.Root)
	if err != nil {
		return err
	}
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
	}()
	if c.BootID == "" {
		b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if e != nil {
			return e
		}
		c.BootID = strings.TrimSpace(string(b))
		if c.BootID == "" {
			return errors.New("boot identity unavailable")
		}
	}
	hostIdentity := c.Host + "\n" + c.BootID
	hostPath := filepath.Join(c.Root, ".celld-launcher-host")
	old, err := os.ReadFile(hostPath)
	if err == nil && string(old) != hostIdentity {
		s.phase("Blocked", errors.New("cross-host volume reuse is unqualified"))
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := persistHost(c.Root, hostPath, []byte(hostIdentity)); err != nil {
			return err
		}
	}
	// New launches never use the runtime's preserve-mode generation override.
	if _, err := os.Stat(filepath.Join(c.Root, ".clean-reload.json")); !errors.Is(err, os.ErrNotExist) {
		s.phase("Blocked", errors.New("unqualified clean-reload marker"))
		<-ctx.Done()
		return ctx.Err()
	}
	s.phase("Spacing", nil)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.Spacing):
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Command[0], c.Command[1:]...) // exact child lifetime is supervised below
	cmd.SysProcAttr = processAttributes()
	cmd.ExtraFiles = []*os.File{lock} // FD3 shares flock ownership through fork and exec.
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CELLD_REEXEC_PROBE_SIGNING_KEY=") && !strings.HasPrefix(entry, "CELLD_TEST_GENERATION=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "CELLD_REEXEC_PROBE_SIGNING_KEY="+hex.EncodeToString(priv.Seed()))
	// Keep the spawning OS thread alive for Linux Pdeathsig semantics.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.state.PID = cmd.Process.Pid
	s.state.Phase = "Running"
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Signal the exact os.Process handle, never a numeric PID/group that could
	// be recycled after Wait. Descendants retain FD3 and must release it before
	// the independent lock acquisition can certify completion.
	select {
	case <-s.stop:
	case <-ctx.Done():
	case err := <-done:
		// An unsolicited exit cannot certify an operation. Retain the lock and
		// report failure; no automatic child restart resurrects an invocation.
		s.phase("ExitedUnrequested", err)
		<-ctx.Done()
		return ctx.Err()
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	// Do not use LOCK_UN: that unlocks the shared open-file description while
	// descendants may still possess it. Closing only our FD preserves their
	// ownership. Successful independent reacquisition proves all inherited
	// holders released it; otherwise remain blocked, never issue a certificate.
	if err := lock.Close(); err != nil {
		return err
	}
	lock = nil
	proofCtx, proofCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer proofCancel()
	proof, err := waitLock(proofCtx, c.Root)
	if err != nil {
		s.phase("Blocked", fmt.Errorf("child exited but inherited lock remains: %w", err))
		<-ctx.Done()
		return ctx.Err()
	}
	lock = proof
	s.phase("Stopped", nil)
	<-ctx.Done()
	return nil
}

// Persist the restrictive host identity before allowing the first disk opener.
// Its existence never serves as a positive stopped-process certificate.
func persistHost(root, path string, body []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if e != nil {
		return e
	}
	if _, e = f.Write(body); e != nil {
		_ = f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		_ = f.Close()
		return e
	}
	if e := f.Close(); e != nil {
		return e
	}
	directory, e := os.Open(root)
	if e != nil {
		return e
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
