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

	"github.com/ewhauser/celld-operator/internal/runtime/controlplane"
)

type Config struct {
	Root, Address, PodUID, Node, Host, BootID string
	Key                                       []byte
	Command                                   []string
	Spacing                                   time.Duration
	// Grace is the pod's terminationGracePeriodSeconds: the whole span kubelet
	// allows between its SIGTERM and its SIGKILL. Zero selects the 30 second
	// default. Both bounds an unrequested termination spends are derived from
	// it by terminationBudget, so their sum stays inside the grace period.
	Grace time.Duration
	// StopGrace overrides only the wait for the child after SIGTERM before
	// SIGKILL. Zero derives that wait from Grace. Tests use it to escalate
	// quickly; production leaves it zero and passes Grace instead.
	StopGrace      time.Duration
	Stdout, Stderr io.Writer
	// Control defaults to the direct, node-local typed client. Tests may supply
	// its transport; the launcher never implements runtime wire decoding.
	Control *controlplane.Client `json:"-"`
}

// defaultGrace mirrors the operator's default terminationGracePeriodSeconds
// (fleet.DefaultTerminationGrace); the controller passes the fleet's own value
// when it differs.
const defaultGrace = 30 * time.Second

// graceMargin reserves the tail of the pod's grace period for the work that
// follows the lock proof: reacquiring the lock, reading back its token and
// linking the retired-disk marker. It matches the operator's
// terminationGraceHeadroom.
const graceMargin = 5 * time.Second

// maxLockProof caps the derived inherited-lock wait. Descendants that survived
// the child's SIGKILL only have to close FD3; ten seconds is generous, and any
// grace left beyond that is better spent letting the child stop gracefully.
const maxLockProof = 10 * time.Second

// requestExpiryBound is the longest future expiry a request may carry: the
// controller uses three seconds, plus tolerance for clock skew between pods.
const requestExpiryBound = 10 * time.Second

// terminationBudget splits a pod grace period into the two waits an
// unrequested termination spends in sequence: stop is the wait for the child
// after SIGTERM before SIGKILL, and proof caps the wait for inherited lock
// holders to release afterwards. stop+proof+graceMargin == grace, so the
// launcher gives up (fail-closed: no certificate, no retired marker) before
// kubelet's SIGKILL rather than after it. Requested stops ignore proof and
// wait indefinitely: the pod lives until the controller decrements.
func terminationBudget(grace time.Duration) (stop, proof time.Duration) {
	if grace <= 0 {
		grace = defaultGrace
	}
	// A grace period too small to split leaves kubelet's SIGKILL as the only
	// outer bound; keep both waits usable rather than zero.
	usable := max(grace-graceMargin, time.Second)
	proof = min(usable/2, maxLockProof)
	return usable - proof, proof
}

type supervisor struct {
	mu       sync.Mutex
	state    State
	stop     chan struct{}
	stopping bool
	key      []byte
}

func (s *supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/v2" {
		http.Error(w, "unsupported", 404)
		return
	}
	var req Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || len(req.Nonce) != 64 || !Verify(s.key, "request", req, r.Header.Get("X-Celld-MAC")) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Operation != "" {
		now := time.Now().UnixMilli()
		// Every stop request expires: the controller bounds NotAfterMS to a few
		// seconds and to its operation deadline, so a late replay is refused
		// regardless of phase. A new operation binds only to a Running child;
		// a supervisor already stopping for another reason (or stopped) answers
		// only the operation it accepted while Running, never a later one.
		expired := req.NotAfterMS <= now || req.NotAfterMS > now+requestExpiryBound.Milliseconds()
		newBinding := s.state.Operation == ""
		if expired || req.Generation != s.state.Generation || (s.state.Operation != "" && s.state.Operation != req.Operation) || (newBinding && (s.state.Phase != "Running" || req.DeadlineMS <= now || req.DeadlineMS > now+int64((24*time.Hour)/time.Millisecond))) {
			http.Error(w, "invocation changed or unavailable", http.StatusConflict)
			return
		}
		if newBinding {
			s.state.Operation = req.Operation
			s.state.DeadlineMS = req.DeadlineMS
		}
		if !s.stopping {
			s.stopping = true
			s.state.Phase = "Draining"
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
	s := &supervisor{state: State{PodUID: c.PodUID, Node: c.Node, Host: c.Host, Invocation: Nonce(), Generation: hex.EncodeToString(pub), Phase: "WaitingForExclusiveVolume"}, stop: make(chan struct{}), key: c.Key}
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
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
	}()
	token, err := lockToken(lock)
	if err != nil {
		return err
	}
	// Retirement is a one-way deny rule for the disk. Neither a stale kubelet
	// nor a replacement Pod UID may reopen it after proof has been captured.
	// Growth always receives a fresh disk; no marker history is needed.
	retiredPath := retiredDiskPath(c.Root)
	if _, err := os.Stat(retiredPath); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		// A permanent deny marker excludes all successors independently of
		// flock. Release promptly so the retiring supervisor can finish its
		// independent inherited-descriptor proof if we won that acquisition.
		if err := lock.Close(); err != nil {
			return err
		}
		lock = nil
		s.phase("Blocked", errors.New("disk was durably retired"))
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
	// Local flock cannot prove exclusion on a different host incarnation.
	// Cross-host reuse is blocked until the disk-policy executor supplies its
	// replacement mechanism. No launcher handoff grant or history is retained.
	if err == nil && string(old) != hostIdentity {
		s.phase("Blocked", errors.New("cross-host disk reuse requires disk-policy cutover"))
		<-ctx.Done()
		return ctx.Err()
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := persistHost(c.Root, hostPath, []byte(hostIdentity)); err != nil {
			return err
		}
	}
	diskPath := filepath.Join(c.Root, ".celld-launcher-disk")
	disk, err := os.ReadFile(diskPath)
	if errors.Is(err, os.ErrNotExist) {
		disk = []byte(Nonce())
		if err := persistHost(c.Root, diskPath, disk); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(disk) != 64 {
		return errors.New("invalid disk identity")
	}
	s.mu.Lock()
	s.state.BootID, s.state.DiskID = c.BootID, string(disk)
	s.mu.Unlock()
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
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	reaperCtx, stopReaper := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReaper()
	go reapOrphans(reaperCtx, cmd.Process.Pid)
	// Signal the exact os.Process handle, never a numeric PID/group that could
	// be recycled after Wait. Descendants retain FD3 and must release it before
	// the independent lock acquisition can certify completion.
	select {
	case <-s.stop:
		s.mu.Lock()
		operation, generation, deadline := s.state.Operation, s.state.Generation, s.state.DeadlineMS
		s.mu.Unlock()
		removalCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(deadline))
		go func() {
			select {
			case <-done:
				cancel()
			case <-removalCtx.Done():
			}
		}()
		err := s.captureRemoval(removalCtx, c.Control, controlplane.Target{IP: "127.0.0.1", Node: c.Node, Generation: generation}, operation, done)
		cancel()
		if err != nil {
			s.phase("Failed", err)
			// Preserve recovery service after failure or ambiguous HTTP results.
			// At the fixed deadline (or pod termination) the exact child can be
			// stopped, but that can never manufacture a data-safe result.
			timer := time.NewTimer(time.Until(time.UnixMilli(deadline)))
			select {
			case <-ctx.Done():
			case <-done:
			case <-timer.C:
			}
			timer.Stop()
		} else {
			s.phase("Terminating", nil)
		}
	case <-ctx.Done():
		// Termination the controller did not request. Close the binding window
		// now: a stop request arriving during shutdown must not adopt this exit.
		s.mu.Lock()
		s.stopping = true
		s.state.Phase = "Terminating"
		s.mu.Unlock()
	case <-done:
		// An unsolicited exit cannot certify an operation. Retain the lock and
		// report failure; no automatic child restart resurrects an invocation.
		s.mu.Lock()
		s.state.ChildExited = true
		s.mu.Unlock()
		s.phase("ExitedUnrequested", waitErr)
		<-ctx.Done()
		return ctx.Err()
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	stopGrace, proofBound := terminationBudget(c.Grace)
	if c.StopGrace > 0 {
		stopGrace = c.StopGrace
	}
	select {
	case <-done:
	case <-time.After(stopGrace):
		_ = cmd.Process.Kill()
		<-done
	}
	s.mu.Lock()
	s.state.ChildExited = true
	s.mu.Unlock()
	// Persist negative authority before releasing our descriptor: another
	// launcher must never open the disk in the gap before independent lock
	// reacquisition. This marker cannot reconstruct positive stop authority.
	if err := persistHost(c.Root, retiredPath, []byte(c.PodUID)); err != nil {
		s.phase("Blocked", fmt.Errorf("cannot durably deny retired disk restart: %w", err))
		<-ctx.Done()
		return ctx.Err()
	}
	s.mu.Lock()
	s.state.RestartDenied = true
	s.mu.Unlock()
	// Do not use LOCK_UN: that unlocks the shared open-file description while
	// descendants may still possess it. Closing only our FD preserves their
	// ownership. Successful independent reacquisition proves all inherited
	// holders released it; otherwise remain blocked, never issue a certificate.
	if err := lock.Close(); err != nil {
		return err
	}
	lock = nil
	// Descendants may hold FD3 a little longer than the child: SIGKILL reaches
	// the child process itself, not its group, so a grandchild can outlive it.
	// Keep waiting for a requested stop: the pod exists until the controller
	// decrements, and certifying early would be wrong. After an unrequested
	// termination the wait is capped at proofBound, which with stopGrace and
	// graceMargin fits inside the pod's grace period. Report the wait so
	// operators can see it.
	s.phase("ReleasingInheritedLock", nil)
	proofCtx, proofCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer proofCancel()
	if ctx.Err() != nil {
		proofCtx, proofCancel = context.WithTimeout(context.WithoutCancel(ctx), proofBound)
		defer proofCancel()
	} else {
		go func() {
			select {
			case <-ctx.Done():
				proofCancel()
			case <-proofCtx.Done():
			}
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
	s.mu.Lock()
	s.state.InheritedLockReleased = true
	switch {
	case s.state.RuntimeDataSafe():
		s.state.Phase = "Stopped"
	case s.state.Operation != "":
		s.state.Phase = "Failed"
	default:
		s.state.Phase = "Terminated"
	}
	s.mu.Unlock()
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

func retiredDiskPath(root string) string {
	return filepath.Join(root, ".celld-launcher-retired")
}

// captureRemoval consumes only the typed strict API, independently of actor load.
// A POST acknowledgement never sets DataSafe, even if it reports completion.
func (s *supervisor) captureRemoval(ctx context.Context, client *controlplane.Client, target controlplane.Target, operation string, exited <-chan struct{}) error {
	if client == nil {
		client = controlplane.New(nil)
	}
	if _, err := client.RemoveDisk(ctx, target, operation); err != nil {
		return fmt.Errorf("strict shutdown request: %w", err)
	}
	for {
		status, err := client.RemovalStatus(ctx, target, operation)
		if err != nil {
			return fmt.Errorf("strict shutdown result unavailable: %w", err)
		}
		// A deadline or child exit racing the HTTP response invalidates capture.
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-exited:
			return errors.New("child exited before strict result capture")
		default:
		}
		result := RemovalResult{Operation: status.OperationID, Generation: status.Generation,
			Mode: "remove-disk", Phase: status.Phase, ControlOnly: status.ControlOnly, DataSafe: status.DataSafe()}
		if status.Blocker != nil {
			result.Blocker = *status.Blocker
		}
		s.mu.Lock()
		s.state.Removal = result
		s.mu.Unlock()
		if status.DataSafe() {
			return nil
		}
		if status.Phase == "failed" {
			return fmt.Errorf("strict shutdown failed: %s", result.Blocker)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
