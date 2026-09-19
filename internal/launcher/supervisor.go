package launcher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
	// StopGrace bounds the wait for the child after SIGTERM before SIGKILL. Zero
	// selects the default that fits the 30 second pod grace period.
	StopGrace      time.Duration
	Stdout, Stderr io.Writer
}

const defaultStopGrace = 25 * time.Second

// requestExpiryBound is the longest future expiry a request may carry: the
// controller uses three seconds, plus tolerance for clock skew between pods.
const requestExpiryBound = 10 * time.Second

// lockProofBound caps the wait for inherited lock holders to release after a
// termination the controller did not request; kubelet will kill this process
// soon anyway and no certificate is owed. Requested stops wait indefinitely.
const lockProofBound = 20 * time.Second

type supervisor struct {
	mu              sync.Mutex
	state           State
	stop            chan struct{}
	stopping        bool
	key             []byte
	handoff         chan struct{}
	handoffAccepted bool
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
	if req.Handoff != nil {
		h := req.Handoff
		if req.Operation != "" || req.NotAfterMS <= time.Now().UnixMilli() || req.NotAfterMS > time.Now().Add(3*time.Second).UnixMilli() || s.state.Phase != "WaitingForHandoff" || h.Invocation != s.state.Invocation || h.Generation != s.state.Generation || h.PodUID != s.state.PodUID || h.Host != s.state.Host || h.BootID != s.state.BootID || h.DiskID != s.state.DiskID || h.PreviousHost != s.state.PreviousHost || h.DiskID == "" {
			http.Error(w, "handoff association changed or expired", http.StatusConflict)
			return
		}
		if !s.handoffAccepted {
			s.handoffAccepted = true
			close(s.handoff)
		}
	}
	if req.Operation != "" {
		now := time.Now().UnixMilli()
		// Every stop request expires: the controller bounds NotAfterMS to a few
		// seconds and to its operation deadline, so a late replay is refused
		// regardless of phase. A new operation binds only to a Running child;
		// a supervisor already stopping for another reason (or stopped) answers
		// only the operation it accepted while Running, never a later one.
		expired := req.NotAfterMS <= now || req.NotAfterMS > now+requestExpiryBound.Milliseconds()
		newBinding := s.state.Operation == ""
		if expired || req.Generation != s.state.Generation || (s.state.Operation != "" && s.state.Operation != req.Operation) || (newBinding && s.state.Phase != "Running") {
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
func openLock(root string, create bool) (*os.File, error) {
	path := filepath.Join(root, ".celld-launcher.lock")
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, e := os.OpenFile(path, flags, 0o600)
	if errors.Is(e, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", errLockReplaced, e)
	}
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		return nil, e
	}
	// A lock on an unlinked inode excludes nobody. Certify only when the held
	// descriptor is still the file every other opener will reach. Inode numbers
	// are reused immediately on common Linux filesystems, so callers also compare
	// the token stored inside the file (see lockToken).
	held, e := f.Stat()
	if e != nil {
		_ = f.Close()
		return nil, e
	}
	current, e := os.Stat(path)
	if e != nil || !os.SameFile(held, current) {
		_ = f.Close()
		return nil, errLockReplaced
	}
	return f, nil
}

var errLockReplaced = errors.New("lock file was unlinked or replaced while held")

// lockToken returns the random identity stored in the lock file, writing one
// on first use. A recreated file has a different token, which no inode
// comparison can promise on filesystems that recycle inode numbers.
func lockToken(f *os.File) (string, error) {
	b, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return "", err
	}
	if len(b) == 64 {
		return string(b), nil
	}
	if len(b) != 0 {
		return "", errors.New("lock file carries unexpected content")
	}
	token := Nonce()
	if _, err := f.WriteAt([]byte(token), 0); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return token, nil
}

func waitLock(ctx context.Context, root string, create bool) (*os.File, error) {
	for {
		f, e := openLock(root, create)
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
	s := &supervisor{state: State{PodUID: c.PodUID, Node: c.Node, Host: c.Host, Invocation: Nonce(), Generation: hex.EncodeToString(pub), Phase: "WaitingForExclusiveVolume"}, stop: make(chan struct{}), handoff: make(chan struct{}), key: c.Key}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second}
	defer func() { _ = server.Close() }()
	go func() { _ = server.Serve(listener) }()
	lock, err := waitLock(ctx, c.Root, true)
	if err != nil {
		return err
	}
	token, err := lockToken(lock)
	if err != nil {
		return err
	}
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
	}()
	// Retirement is a one-way deny rule for this Kubernetes pod identity.
	// A stale kubelet may restart a container after an observed stop. It must
	// never resurrect that pod's writer, even after later reuse of this disk.
	retiredPath := retiredPodPath(c.Root, c.PodUID)
	if _, err := os.Stat(retiredPath); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		s.phase("Blocked", errors.New("pod identity was durably retired"))
		<-ctx.Done()
		return ctx.Err()
	}
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && len(old) == 0 {
		// Identity files are written atomically, so an empty file is not a
		// half-written record from this code; treat it as corruption, never as
		// evidence about another host.
		s.phase("Blocked", errors.New("host identity file is empty or corrupt"))
		<-ctx.Done()
		return ctx.Err()
	}
	// A disk nonce binds controller authority to this mounted filesystem. Existing
	// legacy disks can acquire one only on their original host incarnation.
	diskPath := filepath.Join(c.Root, ".celld-launcher-disk")
	disk, diskErr := os.ReadFile(diskPath)
	if errors.Is(diskErr, os.ErrNotExist) {
		if err == nil && string(old) != hostIdentity {
			s.phase("Blocked", errors.New("legacy disk has no transferable identity"))
			<-ctx.Done()
			return ctx.Err()
		}
		disk = []byte(Nonce())
		if e := persistHost(c.Root, diskPath, disk); e != nil {
			return e
		}
	} else if diskErr != nil {
		return diskErr
	}
	if len(disk) != 64 {
		return errors.New("invalid disk identity")
	}
	s.mu.Lock()
	s.state.BootID = c.BootID
	s.state.DiskID = string(disk)
	s.mu.Unlock()
	if err == nil && string(old) != hostIdentity {
		s.mu.Lock()
		s.state.PreviousHost = string(old)
		s.state.Phase = "WaitingForHandoff"
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.handoff:
		}
		// No child exists yet. Commit the new host before spacing or spawning. A
		// replay cannot authorize another host or a new invocation after a crash.
		if e := replaceHost(c.Root, hostPath, []byte(hostIdentity)); e != nil {
			return e
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if e := persistHost(c.Root, hostPath, []byte(hostIdentity)); e != nil {
			return e
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
	reaperCtx, stopReaper := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReaper()
	go reapOrphans(reaperCtx, cmd.Process.Pid)
	// Signal the exact os.Process handle, never a numeric PID/group that could
	// be recycled after Wait. Descendants retain FD3 and must release it before
	// the independent lock acquisition can certify completion.
	select {
	case <-s.stop:
	case <-ctx.Done():
		// Termination the controller did not request. Close the binding window
		// now: a stop request arriving during shutdown must not adopt this exit.
		s.mu.Lock()
		s.stopping = true
		s.state.Phase = "Terminating"
		s.mu.Unlock()
	case err := <-done:
		// An unsolicited exit cannot certify an operation. Retain the lock and
		// report failure; no automatic child restart resurrects an invocation.
		s.phase("ExitedUnrequested", err)
		<-ctx.Done()
		return ctx.Err()
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	stopGrace := c.StopGrace
	if stopGrace <= 0 {
		stopGrace = defaultStopGrace
	}
	select {
	case <-done:
	case <-time.After(stopGrace):
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
	// Descendants may hold FD3 a little longer than the child. Keep waiting for
	// a requested stop: the pod exists until the controller decrements, and
	// certifying early would be wrong. Report the wait so operators can see it.
	s.phase("ReleasingInheritedLock", nil)
	proofCtx, proofCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer proofCancel()
	if ctx.Err() != nil {
		proofCtx, proofCancel = context.WithTimeout(context.WithoutCancel(ctx), lockProofBound)
		defer proofCancel()
	} else {
		go func() {
			<-ctx.Done()
			proofCancel()
		}()
	}
	proof, err := waitLock(proofCtx, c.Root, false)
	if err != nil {
		s.phase("Blocked", fmt.Errorf("child exited but inherited lock remains: %w", err))
		<-ctx.Done()
		return ctx.Err()
	}
	lock = proof
	// The proof must be the very file the child inherited. If the path was
	// unlinked or replaced meanwhile, locking the new file says nothing about
	// holders of the old one, so no certificate can be issued.
	if proofToken, e := lockToken(proof); e != nil || proofToken != token {
		s.phase("Blocked", errLockReplaced)
		<-ctx.Done()
		return ctx.Err()
	}
	// Persist only negative authority before publishing the live receipt. This
	// file cannot certify termination to another process or reconstruct Stopped.
	// A failure here leaves the supervisor blocked, with no positive receipt.
	if err := persistHost(c.Root, retiredPath, []byte(c.PodUID)); err != nil {
		s.phase("Blocked", fmt.Errorf("cannot durably deny retired pod restart: %w", err))
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	s.state.RestartDenied = true
	s.mu.Unlock()
	s.phase("Stopped", nil)
	<-ctx.Done()
	return nil
}

// Persist the restrictive host identity before allowing the first disk opener.
// Its existence never serves as a positive stopped-process certificate.
func persistHost(root, path string, body []byte) error {
	// Write the full record to a private temporary name, then link it into place.
	// Link fails if the path exists, preserving create-exclusive semantics, and
	// never exposes a partially written file to a later opener.
	temporary := path + ".tmp." + Nonce()
	f, e := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if e != nil {
		return e
	}
	if _, e = f.Write(body); e != nil {
		_ = f.Close()
		_ = os.Remove(temporary)
		return e
	}
	if e = f.Sync(); e != nil {
		_ = f.Close()
		_ = os.Remove(temporary)
		return e
	}
	if e := f.Close(); e != nil {
		_ = os.Remove(temporary)
		return e
	}
	if e := os.Link(temporary, path); e != nil {
		_ = os.Remove(temporary)
		return e
	}
	_ = os.Remove(temporary)
	directory, e := os.Open(root)
	if e != nil {
		return e
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func replaceHost(root, path string, body []byte) error {
	temporary := path + "." + Nonce()
	if err := persistHost(root, temporary, body); err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func retiredPodPath(root, podUID string) string {
	hash := sha256.Sum256([]byte(podUID))
	return filepath.Join(root, ".celld-launcher-retired-"+hex.EncodeToString(hash[:]))
}
