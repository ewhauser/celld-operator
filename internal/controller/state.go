package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/capacity"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const stateKey = "celld.eric.dev/current-operation"
const maxStateBytes = 180 * 1024

// fleetState is all the operator persists for a fleet: capacity-policy state,
// which exists once the fleet sets spec.capacity, kept on the storage
// reservation. Workload identity, replica counts, images and disks are read
// from the cluster on every reconcile (ADR 0024), and the annotation is
// removed when there is no capacity state to keep. The reservation's immutable
// Spec independently owns the bucket forever.
type fleetState struct {
	Version  int
	FleetUID types.UID
	Capacity *capacity.State `json:",omitempty"`
	// Operation is a strict operation recorded before ADR 0023. It is read so
	// currentState can report it superseded, and is never written back.
	// Other fields earlier releases wrote are ignored and dropped on save.
	Operation *legacyOperation `json:",omitempty"`
}

// legacyOperation is the identity of a strict operation written before ADR 0023.
type legacyOperation struct{ ID, Kind string }

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
		return nil, errors.New("persisted fleet state exceeds bounded state limit")
	}
	var s fleetState
	d := json.NewDecoder(bytes.NewBufferString(raw))
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
	if s.Version != 1 || s.FleetUID == "" {
		return errors.New("invalid persisted fleet state")
	}
	if s.Capacity != nil && (len(s.Capacity.Load) > 100 || len(s.Capacity.Stamps) > 100 || (s.Capacity.Addition != nil && len(s.Capacity.Addition.Before) > 100)) {
		return errors.New("capacity working set exceeds fleet bound")
	}
	return nil
}

// saveState writes s's capacity history to the reservation, or removes the
// annotation when there is none. Nothing is written when the annotation
// already holds exactly that.
func (r *Reconciler) saveState(ctx context.Context, res *fleet.CelldStorageReservation, s *fleetState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	want := ""
	if s != nil && s.Capacity != nil {
		kept := fleetState{Version: s.Version, FleetUID: s.FleetUID, Capacity: s.Capacity}
		if err := validateState(&kept); err != nil {
			return err
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		if len(b) > maxStateBytes {
			return errors.New("persisted fleet state is oversized")
		}
		want = string(b)
	}
	if res.Annotations[stateKey] == want {
		return nil
	}
	if want == "" {
		delete(res.Annotations, stateKey)
	} else {
		if res.Annotations == nil {
			res.Annotations = map[string]string{}
		}
		res.Annotations[stateKey] = want
	}
	// Never retry a reservation write using a refreshed resourceVersion.
	return r.Update(ctx, res)
}
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
	return fmt.Sprintf("Persisted capacity-policy state uses %d of %d bytes", s.bytes, maxStateBytes)
}
