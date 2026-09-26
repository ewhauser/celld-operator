// Package controlplane reads the private, node-local celld HTTP API. The
// operator only observes /state; it never asks a node to shut down, reload or
// remove its disk.
package controlplane

import (
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

// shutdownSchema is the version of the shutdown object the fork adds to /state
// (crates/celld/disk_removal.rs on ewhauser/celld). Capacity requires it and
// the runtime generation it reports.
const shutdownSchema = 1

var ErrUnsupported = errors.New("runtime capability unsupported")
var ErrIdentity = errors.New("runtime identity mismatch or unavailable")

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
type Target struct{ IP string }

type Client struct{ http *http.Client }

// New accepts a transport for tests; nil uses a direct connection without proxy
// discovery. No credentials are invented for the unauthenticated internal API.
func New(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, MaxConnsPerHost: 2, MaxIdleConns: 100, IdleConnTimeout: 30 * time.Second}
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: callTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// readState fetches /state from the node's internal listener and requires a
// complete, single JSON object.
func (c *Client) readState(ctx context.Context, target Target) ([]byte, error) {
	if net.ParseIP(target.IP) == nil {
		return nil, errors.New("exact node IP required")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(target.IP, "8081")+"/state", http.NoBody)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: response.StatusCode}
	}
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxResponse {
		return nil, errors.New("runtime response exceeds budget")
	}
	if err := validObject(data); err != nil {
		return nil, err
	}
	return data, nil
}

// State is the runtime identity a node reports in /state, with the raw
// response kept for the load decoder.
type State struct {
	// SchemaVersion is the shutdown object's version, nil when it is absent.
	SchemaVersion *uint32
	// Generation is the runtime process identity, read only from schema 1.
	Generation string
	ReceivedAt time.Time
	raw        []byte
}

// Capacity requires the new control-plane identity/schema before interpreting
// load. Old runtime images are not a supported operator execution path.
func (s State) Capacity(maxAge time.Duration) (Load, error) {
	if s.SchemaVersion == nil || *s.SchemaVersion != shutdownSchema {
		return Load{}, ErrUnsupported
	}
	if s.Generation == "" {
		return Load{}, ErrIdentity
	}
	return parseLoad(s.raw, s.ReceivedAt, s.ReceivedAt, maxAge)
}

func (c *Client) State(ctx context.Context, target Target) (State, error) {
	data, err := c.readState(ctx, target)
	if err != nil {
		return State{}, err
	}
	state, err := decodeState(data)
	if err != nil {
		return State{}, err
	}
	state.ReceivedAt = time.Now()
	state.raw = data
	return state, nil
}

// decodeState inspects the shutdown object's version first, so an unknown
// future shape neither breaks load collection nor supplies an identity.
func decodeState(data []byte) (State, error) {
	var wire struct {
		Shutdown json.RawMessage `json:"shutdown"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return State{}, err
	}
	if len(wire.Shutdown) == 0 || string(wire.Shutdown) == "null" {
		return State{}, nil
	}
	var version struct {
		SchemaVersion *uint32 `json:"schema_version"`
	}
	if err := json.Unmarshal(wire.Shutdown, &version); err != nil {
		return State{}, err
	}
	state := State{SchemaVersion: version.SchemaVersion}
	if version.SchemaVersion == nil || *version.SchemaVersion != shutdownSchema {
		return state, nil
	}
	var identity struct {
		Generation string `json:"runtime_generation"`
	}
	if err := json.Unmarshal(wire.Shutdown, &identity); err != nil {
		return State{}, err
	}
	state.Generation = identity.Generation
	return state, nil
}
