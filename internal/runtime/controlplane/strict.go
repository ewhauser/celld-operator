package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
)

// This wire adapter follows crates/celld/disk_removal.rs::State::snapshot and
// main.rs::handle_internal on codex/strict-disk-removal (v0.5.1 base).
// Stock releases have no shutdown extension. No S3 fallback exists here.
const strictSchema = 1

type Identity struct{ Generation string }

// Nil fields mean absent, false means explicitly unsupported, and versions
// other than 1 remain unknown. None of those authorize strict operations.
type Capabilities struct {
	SchemaVersion     *uint32
	StrictDiskRemoval *bool
}

func (c Capabilities) SupportsRemoveDisk() bool {
	return c.SchemaVersion != nil && *c.SchemaVersion == strictSchema && c.StrictDiskRemoval != nil && *c.StrictDiskRemoval
}

type ShutdownStatus struct {
	OperationID string
	Generation  string
	Phase       string
	Blocker     *string
	ControlOnly bool
	dataSafe    bool
}

// DataSafe is set only after fresh capability, generation, operation, phase and
// control-only validation. It never proves termination or restart exclusion.
func (s ShutdownStatus) DataSafe() bool { return s.dataSafe }

type shutdownWire struct {
	SchemaVersion *uint32 `json:"schema_version"`
	Generation    string  `json:"runtime_generation"`
	Capabilities  struct {
		StrictDiskRemoval *bool `json:"strict_disk_removal"`
	} `json:"capabilities"`
	ControlOnly *bool `json:"control_only"`
	Operation   *struct {
		OperationID string          `json:"operation_id"`
		Generation  string          `json:"expected_generation"`
		Mode        string          `json:"mode"`
		Phase       string          `json:"phase"`
		Blocker     json.RawMessage `json:"blocker"`
	} `json:"operation"`
}

func decodeShutdown(data []byte) (State, error) {
	// Inspect the version first. Unknown future wire shapes must not break load
	// collection, nor be interpreted using today's completion semantics.
	var version struct {
		SchemaVersion *uint32 `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return State{}, err
	}
	state := State{Capabilities: Capabilities{SchemaVersion: version.SchemaVersion}}
	if version.SchemaVersion == nil || *version.SchemaVersion != strictSchema {
		return state, nil
	}
	var wire shutdownWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return State{}, err
	}
	state.Identity.Generation = wire.Generation
	state.Capabilities.StrictDiskRemoval = wire.Capabilities.StrictDiskRemoval
	if wire.Operation == nil {
		return state, nil
	}
	op := wire.Operation
	if wire.ControlOnly == nil || len(op.Blocker) == 0 || op.Mode != "remove-disk" || !operationID.MatchString(op.OperationID) || op.Generation == "" {
		return State{}, errors.New("incomplete shutdown operation")
	}
	var blocker *string
	if err := json.Unmarshal(op.Blocker, &blocker); err != nil {
		return State{}, err
	}
	status := ShutdownStatus{OperationID: op.OperationID, Generation: op.Generation, Phase: op.Phase, Blocker: blocker, ControlOnly: *wire.ControlOnly}
	switch status.Phase {
	case "draining", "failed":
	case "data_safe":
		if !status.ControlOnly || status.Blocker != nil {
			return State{}, errors.New("invalid data-safe result")
		}
	default:
		return State{}, errors.New("unknown shutdown phase")
	}
	state.Shutdown = &status
	return state, nil
}
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
	return decodeShutdown(wire.Shutdown)
}

var operationID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func validateOperation(target Target, operation string) error {
	if target.Node == "" || target.Generation == "" {
		return ErrIdentity
	}
	if len(target.Generation) > 256 {
		return errors.New("runtime generation exceeds request budget")
	}
	if !operationID.MatchString(operation) {
		return errors.New("invalid shutdown operation ID")
	}
	return nil
}
func boundStatus(state State, target Target, operation string) (ShutdownStatus, error) {
	if !state.Capabilities.SupportsRemoveDisk() {
		return ShutdownStatus{}, ErrUnsupported
	}
	if state.Identity.Generation != target.Generation {
		return ShutdownStatus{}, ErrIdentity
	}
	status := state.Shutdown
	if status == nil || status.OperationID != operation || status.Generation != target.Generation {
		return ShutdownStatus{}, errors.New("shutdown operation absent or mismatched")
	}
	result := *status
	result.dataSafe = result.Phase == "data_safe" && result.ControlOnly && result.Blocker == nil
	return result, nil
}
func (c *Client) RemovalStatus(ctx context.Context, target Target, operation string) (ShutdownStatus, error) {
	if err := validateOperation(target, operation); err != nil {
		return ShutdownStatus{}, err
	}
	discovery := target
	discovery.Generation = ""
	state, err := c.State(ctx, discovery)
	if err != nil {
		return ShutdownStatus{}, err
	}
	return boundStatus(state, target, operation)
}
func (c *Client) RemoveDisk(ctx context.Context, target Target, operation string) (Acceptance, error) {
	if err := validateOperation(target, operation); err != nil {
		return Acceptance{}, err
	}
	// Capability preflight prevents an old server ignoring mode=remove-disk and
	// executing an ordinary shutdown. The POST independently checks generation.
	discovery := target
	discovery.Generation = ""
	state, err := c.State(ctx, discovery)
	if err != nil {
		return Acceptance{}, err
	}
	if !state.Capabilities.SupportsRemoveDisk() {
		return Acceptance{}, ErrUnsupported
	}
	if state.Identity.Generation != target.Generation {
		return Acceptance{}, ErrIdentity
	}
	body, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
		Generation  string `json:"expected_generation"`
	}{operation, target.Generation})
	if err != nil {
		return Acceptance{}, err
	}
	data, code, err := c.call(ctx, target, http.MethodPost, "/shutdown?mode=remove-disk", body)
	if err != nil {
		return Acceptance{}, err
	}
	if code != http.StatusAccepted {
		return Acceptance{}, errors.New("strict shutdown was not accepted")
	}
	accepted, err := decodeShutdown(data)
	if err != nil {
		return Acceptance{}, err
	}
	if _, err := boundStatus(accepted, target, operation); err != nil {
		return Acceptance{}, err
	}
	// Even a duplicate POST reporting data_safe is only an acknowledgement here.
	// The caller must observe a fresh matching /state via RemovalStatus.
	return Acceptance{Accepted: true}, nil
}
