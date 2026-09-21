// Package controlplane implements the private, node-local celld HTTP API.
// An accepted shutdown is never evidence that a process or disk can be removed.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const maxResponse = 1 << 20
const callTimeout = 2 * time.Second

var ErrUnsupported = errors.New("runtime capability unsupported")
var ErrIdentity = errors.New("runtime identity mismatch or unavailable")

// TransportError means no complete response was observed. Callers may retry
// reads within their existing deadline; this never makes a mutation safe to
// replay or supplies evidence of runtime completion.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// HTTPError preserves protocol rejection codes without exposing response bodies.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("runtime HTTP status %d", e.StatusCode) }
func (e *HTTPError) Unwrap() error {
	if e.StatusCode == http.StatusNotImplemented {
		return ErrUnsupported
	}
	return nil
}

// Target uses an exact Pod IP, never a load-balanced Service or a redirect.
// Generation is the runtime ownership generation, not deployment.generation.
// Node is caller-supplied Kubernetes identity; the wire exposes only Generation.
// Reads may omit Generation for discovery; strict requests must supply it.
type Target struct{ IP, Node, Generation string }

// Lifecycle is shared by Bucket and PersistentFleet. It supplies observations
// and requests, not Kubernetes termination, fencing, or disk-removal authority.
type Lifecycle interface {
	State(context.Context, Target) (State, error)
	Shutdown(context.Context, Target, ShutdownMode) (Acceptance, error)
	RemoveDisk(context.Context, Target, string) (Acceptance, error)
	RemovalStatus(context.Context, Target, string) (ShutdownStatus, error)
	Reload(context.Context, Target) (ReloadResult, error)
}

type Client struct{ http *http.Client }

// New accepts a transport for tests; nil uses a direct connection without proxy
// discovery. No credentials are invented for the unauthenticated internal API.
func New(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, MaxConnsPerHost: 2, MaxIdleConns: 100, IdleConnTimeout: 30 * time.Second}
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: callTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) call(ctx context.Context, target Target, method, path string, body []byte) ([]byte, int, error) {
	if net.ParseIP(target.IP) == nil {
		return nil, 0, errors.New("exact node IP required")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(target.IP, "8081")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &TransportError{Err: err}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	closeErr := response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, &HTTPError{StatusCode: response.StatusCode}
	}
	if err != nil {
		return nil, response.StatusCode, &TransportError{Err: err}
	}
	if closeErr != nil {
		return nil, response.StatusCode, &TransportError{Err: closeErr}
	}
	if len(data) > maxResponse {
		return nil, response.StatusCode, errors.New("runtime response exceeds budget")
	}
	if err := validObject(data); err != nil {
		return nil, response.StatusCode, err
	}
	return data, response.StatusCode, nil
}

type State struct {
	Identity     Identity
	Capabilities Capabilities
	Shutdown     *ShutdownStatus
	ReceivedAt   time.Time
	raw          []byte
}

// Capacity requires the new control-plane identity/schema before interpreting
// load. Old runtime images are not a supported operator execution path.
func (s State) Capacity(maxAge time.Duration) (Load, error) {
	if s.Capabilities.SchemaVersion == nil || *s.Capabilities.SchemaVersion != strictSchema {
		return Load{}, ErrUnsupported
	}
	if s.Identity.Generation == "" {
		return Load{}, ErrIdentity
	}
	return parseLoad(s.raw, s.ReceivedAt, s.ReceivedAt, maxAge)
}

func (c *Client) State(ctx context.Context, target Target) (State, error) {
	data, code, err := c.call(ctx, target, http.MethodGet, "/state", nil)
	if err != nil {
		return State{}, err
	}
	if code != http.StatusOK {
		return State{}, errors.New("unexpected state status")
	}
	state, err := decodeState(data)
	if err != nil {
		return State{}, err
	}
	if target.Generation != "" && target.Generation != state.Identity.Generation {
		return State{}, ErrIdentity
	}
	state.ReceivedAt = time.Now()
	state.raw = data
	return state, nil
}

type ShutdownMode string

const (
	Ordinary ShutdownMode = "ordinary"
	Preserve ShutdownMode = "preserve"
)

// Acceptance intentionally has no completion flag. Only a fresh, matching
// versioned /state status may describe data safety, and never process death.
type Acceptance struct{ Accepted bool }

func (c *Client) Shutdown(ctx context.Context, target Target, mode ShutdownMode) (Acceptance, error) {
	path := "/shutdown"
	switch mode {
	case Ordinary:
	case Preserve:
		path += "?handoff=preserve"
	default:
		return Acceptance{}, ErrUnsupported
	}
	// Ordinary mutations have no generation precondition. Do not pretend a read
	// before a POST closes the process-replacement race.
	if target.Generation != "" {
		return Acceptance{}, ErrUnsupported
	}
	data, code, err := c.call(ctx, target, http.MethodPost, path, nil)
	if err != nil {
		return Acceptance{}, err
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Acceptance{}, err
	}
	if code != http.StatusOK || !result.OK {
		return Acceptance{}, errors.New("shutdown not accepted")
	}
	return Acceptance{Accepted: true}, nil
}

type ReloadResult struct {
	OK         bool    `json:"ok"`
	Outcome    string  `json:"outcome"`
	Generation *uint64 `json:"generation"`
	Version    *string `json:"version"`
	Prefix     *string `json:"prefix"`
}

func (c *Client) Reload(ctx context.Context, target Target) (ReloadResult, error) {
	if target.Generation != "" {
		return ReloadResult{}, ErrUnsupported
	}
	data, code, err := c.call(ctx, target, http.MethodPost, "/reload", nil)
	if err != nil {
		return ReloadResult{}, err
	}
	var result ReloadResult
	if err := json.Unmarshal(data, &result); err != nil {
		return ReloadResult{}, err
	}
	if code != http.StatusOK || !result.OK || result.Generation == nil || (result.Outcome != "adopted" && result.Outcome != "unchanged") {
		return ReloadResult{}, errors.New("reload did not succeed")
	}
	return result, nil
}

var _ Lifecycle = (*Client)(nil)
