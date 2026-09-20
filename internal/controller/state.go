package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	"github.com/ewhauser/celld-operator/internal/launcher"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const stateKey = "celld.eric.dev/current-operation"
const creationClaimsKey = "celld.eric.dev/creation-claim-uids"
const operationKey = "celld.eric.dev/infrastructure-effect"
const operationBudget = 30 * time.Minute
const maxStateBytes = 180 * 1024

// fleetState contains current Kubernetes ownership and bounded policy state.
// Runtime proof exists only inside Operation, and is discarded at completion.
// The reservation's immutable Spec independently owns the bucket forever.
type fleetState struct {
	Version               int
	FleetUID, WorkloadUID types.UID
	Initial, Applied      int32
	RuntimeImage          string
	Claims                map[string]types.UID
	Operation             *currentOperation    `json:",omitempty"`
	RestartToken          string               `json:",omitempty"`
	Completion            *operationCompletion `json:",omitempty"`
	Capacity              *capacity.State      `json:",omitempty"`
	DiskCleanupPending    bool                 `json:",omitempty"`
}
type operationCompletion struct {
	ID, Kind, Outcome string
	At                time.Time
}
type currentOperation struct {
	ID, Kind, Phase                        string
	StartedAt, Deadline                    time.Time
	FleetUID, WorkloadUID                  types.UID
	From, To                               int32
	SourceImage, TargetImage, RestartToken string
	ManualBaseline                         int32
	Automatic                              bool
	PolicyHash                             string
	PreviousEffect, EffectVersion          string
	Targets                                []operationTarget `json:",omitempty"`
	Blocker                                string            `json:",omitempty"`
}
type operationTarget struct {
	Pod, IP, Container, HostUID string
	PodUID                      types.UID
	Identity                    processIdentity
	Storage                     *volumeIdentity `json:",omitempty"`
	Proof                       *operationProof `json:",omitempty"`
}
type volumeIdentity struct {
	Claim, ClaimVersion, Volume, Handle string
	ClaimUID, VolumeUID                 types.UID
}
type loadedState struct {
	res *fleet.CelldStorageReservation
	j   *fleetState
	err error
}

func (r *Reconciler) hydrate(_ context.Context, res *fleet.CelldStorageReservation) *loadedState {
	s, err := readState(res)
	return &loadedState{res: res, j: s, err: err}
}
func readState(res *fleet.CelldStorageReservation) (*fleetState, error) {
	raw := res.Annotations[stateKey]
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxStateBytes {
		return nil, errors.New("current operation exceeds bounded state limit")
	}
	var s fleetState
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing state content")
	}
	if err := validateState(&s); err != nil {
		return nil, err
	}
	return &s, nil
}
func validateState(s *fleetState) error {
	if s.Version != 1 || s.Initial < 1 || s.Initial > 100 || s.Applied < 0 || s.Applied > 100 || s.FleetUID == "" || s.WorkloadUID == "" || !knownRuntime(s.RuntimeImage) || len(s.Claims) > 100 {
		return errors.New("invalid current infrastructure state")
	}
	for name, uid := range s.Claims {
		if name == "" || uid == "" {
			return errors.New("empty current claim identity")
		}
	}
	if s.Capacity != nil && (len(s.Capacity.Load) > 100 || len(s.Capacity.Stamps) > 100 || (s.Capacity.Addition != nil && len(s.Capacity.Addition.Before) > 100)) {
		return errors.New("capacity working set exceeds fleet bound")
	}
	if o := s.Operation; o != nil {
		if o.ID == "" || o.FleetUID != s.FleetUID || o.WorkloadUID != s.WorkloadUID || o.From != s.Applied || o.From < 0 || o.From > 100 || o.To < 0 || o.To > 100 || o.StartedAt.IsZero() || !o.Deadline.After(o.StartedAt) || o.Deadline.Sub(o.StartedAt) > operationBudget || o.SourceImage != s.RuntimeImage || len(o.Targets) > 100 {
			return errors.New("invalid current operation")
		}
		if o.From < 1 || (o.Kind == "Scale" && (o.To < 1 || o.To == o.From)) || (o.Kind == "Delete" && o.To != 0) || ((o.Kind == "Restart" || o.Kind == "Upgrade") && o.To != o.From) {
			return errors.New("invalid infrastructure transition")
		}
		if (o.Kind == "Scale" || o.Kind == "Restart") && o.TargetImage != o.SourceImage {
			return errors.New("replica transition changed runtime")
		}
		if !knownRuntime(o.TargetImage) {
			return errors.New("operation target is not a fork pin")
		}
		switch o.Kind {
		case "Scale", "Restart", "Upgrade", "Delete":
		default:
			return errors.New("invalid operation kind")
		}
		switch o.Phase {
		case "Intent", "Requesting", "ProofCaptured", "Apply", "Observing", "DeleteClaims", "Resume", "ApplyResume", "Joining", "Blocked":
		default:
			return errors.New("invalid operation phase")
		}
		if o.Kind == "Scale" && o.To < o.From && o.To != o.From-1 {
			return errors.New("scale contraction must remove one member")
		}
		needed := 0
		if o.Kind != "Scale" {
			needed = int(o.From)
		} else if o.To < o.From {
			needed = 1
		}
		if len(o.Targets) != needed {
			return errors.New("incomplete operation working set")
		}
		seen := map[types.UID]bool{}
		for _, t := range o.Targets {
			if t.Pod == "" || t.PodUID == "" || seen[t.PodUID] || t.Identity.Node == "" || t.Identity.Host == "" || t.Identity.PID <= 0 || t.Identity.Generation == "" || t.Identity.Invocation == "" || t.Identity.DiskID == "" || t.Identity.BootID == "" || t.HostUID == "" || t.Container == "" {
				return errors.New("invalid operation target")
			}
			seen[t.PodUID] = true
			if t.Storage != nil && (t.Storage.ClaimUID == "" || t.Storage.VolumeUID == "" || t.Storage.Handle == "" || t.Storage.Claim == "" || t.Storage.Volume == "") {
				return errors.New("invalid target storage")
			}
			if t.Proof != nil && !validProof(o, t, *t.Proof) {
				return errors.New("invalid captured proof")
			}
			if o.Phase != "Intent" && o.Phase != "Requesting" && o.Phase != "Blocked" && t.Proof == nil {
				return errors.New("effect lacks captured proof")
			}
		}
	}
	return nil
}

// Each proof is captured atomically under the target identity after validating
// every response identity and the first-operation deadline. Do not duplicate a
// second full process identity (or its empty discovery result) per member.
type processIdentity struct {
	Node, Host, BootID, Invocation, Generation, DiskID string
	PID                                                int
}
type operationProof struct {
	Removal                                           launcher.RemovalResult
	ChildExited, InheritedLockReleased, RestartDenied bool
}

func processFrom(s launcher.State) processIdentity {
	return processIdentity{Node: s.Node, Host: s.Host, BootID: s.BootID, Invocation: s.Invocation, Generation: s.Generation, DiskID: s.DiskID, PID: s.PID}
}
func proofFrom(s launcher.State) *operationProof {
	return &operationProof{Removal: s.Removal, ChildExited: s.ChildExited, InheritedLockReleased: s.InheritedLockReleased, RestartDenied: s.RestartDenied}
}
func validProof(o *currentOperation, t operationTarget, p operationProof) bool {
	r := p.Removal
	return p.ChildExited && p.InheritedLockReleased && p.RestartDenied && r.Operation == o.ID && r.Generation == t.Identity.Generation && r.Mode == "remove-disk" && r.Phase == "data_safe" && r.ControlOnly && r.DataSafe && r.Blocker == ""
}
func validLauncherProof(o *currentOperation, t operationTarget, p launcher.State) bool {
	return p.RemovalReady() && p.Operation == o.ID && p.DeadlineMS == o.Deadline.UnixMilli() && sameProcess(t, p)
}
func sameProcess(t operationTarget, s launcher.State) bool {
	return t.PodUID == types.UID(s.PodUID) && t.Identity == processFrom(s)
}
func (r *Reconciler) saveState(ctx context.Context, res *fleet.CelldStorageReservation, s *fleetState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateState(s); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return errors.New("bounded current operation is oversized")
	}
	if res.Annotations == nil {
		res.Annotations = map[string]string{}
	}
	res.Annotations[stateKey] = string(b)
	// Never retry a reservation write using a refreshed resourceVersion.
	return r.Update(ctx, res)
}
func stateRendering(s *fleetState) []byte    { b, _ := json.Marshal(s); return b }
func sameState(b []byte, s *fleetState) bool { return bytes.Equal(b, stateRendering(s)) }
func (r *Reconciler) reservationMatches(_ context.Context, f *fleet.CelldFleet, h *loadedState, want fleet.ReservationSpec) bool {
	if h.err != nil || h.res.Spec.InitialReplicas < 1 || h.res.Spec.InitialReplicas > 100 {
		return false
	}
	baseline := f.DeepCopy()
	baseline.Spec.Replicas = h.res.Spec.InitialReplicas
	want.InitialReplicas = h.res.Spec.InitialReplicas
	want.SpecHash = specHash(baseline)
	return want == h.res.Spec
}
func replicas(w client.Object) int32 {
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil {
			return *w.Spec.Replicas
		}
	}
	return -1
}
func setReplicas(w client.Object, n int32) {
	switch w := w.(type) {
	case *appsv1.StatefulSet:
		w.Spec.Replicas = new(n)
	case *appsv1.Deployment:
		w.Spec.Replicas = new(n)
	}
}

type stateFootprint struct{ bytes int }

func measureState(res *fleet.CelldStorageReservation) stateFootprint {
	return stateFootprint{bytes: len(res.Annotations[stateKey])}
}
func (s stateFootprint) nearCapacity() bool { return s.bytes > maxStateBytes/2 }
func stateSizeMessage(s stateFootprint) string {
	return fmt.Sprintf("Current operation and policy state use %d of %d bytes", s.bytes, maxStateBytes)
}
