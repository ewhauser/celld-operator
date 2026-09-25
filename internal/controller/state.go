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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const stateKey = "celld.eric.dev/current-operation"
const maxStateBytes = 180 * 1024

// fleetState is the operator's bounded bookkeeping on the storage reservation:
// current workload identity, the applied count and image, the last restart
// token and disruption, and capacity-policy history. It holds no runtime proof.
// The reservation's immutable Spec independently owns the bucket forever.
type fleetState struct {
	Version               int
	FleetUID, WorkloadUID types.UID
	Initial, Applied      int32
	RuntimeImage          string
	// Claims is retained so state written by earlier releases decodes; it is
	// always empty now.
	Claims map[string]types.UID
	// Operation is an earlier release's strict operation, kept only until
	// loadedCurrent records it as Superseded.
	Operation      *legacyOperation     `json:",omitempty"`
	RestartToken   string               `json:",omitempty"`
	Completion     *operationCompletion `json:",omitempty"`
	Capacity       *capacity.State      `json:",omitempty"`
	LastDisruption time.Time            `json:",omitzero"`
}
type operationCompletion struct {
	ID, Kind, Outcome string
	At                time.Time
}

// legacyOperation reads the identity of a strict operation written before
// ADR 0023 and ignores the rest of its fields.
type legacyOperation struct{ ID, Kind string }

func (o *legacyOperation) UnmarshalJSON(b []byte) error {
	var v struct{ ID, Kind string }
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*o = legacyOperation(v)
	return nil
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
	return nil
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
	if equality.Semantic.DeepEqual(want, h.res.Spec) {
		return true
	}
	want.SpecHash = legacyQualificationSpecHash(baseline)
	return equality.Semantic.DeepEqual(want, h.res.Spec)
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
