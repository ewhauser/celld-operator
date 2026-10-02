package controller

import (
	"bytes"
	"testing"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
)

func TestLegacyQualificationReservation(t *testing.T) {
	f := fixture("legacy", "legacy-bucket", "PersistentFleet")
	want := fleetReservationSpec(f)
	// Hash from the v0.0.2 JSON layout, including qualification immediately
	// before profile. Keep this literal independent of the fallback generator.
	const oldHash = "87fbe80bd9a146d3cef18a6eadc8047024b79ca9d12c6cfbb01a6efb93f923b4"
	if got := legacyQualificationSpecHash(f); got != oldHash {
		t.Fatalf("v0.0.2 hash changed: %s", got)
	}
	if want.SpecHash == oldHash {
		t.Fatal("new reservations must use the current hash")
	}
	res := &fleet.CelldStorageReservation{Spec: want}
	res.Spec.SpecHash = oldHash
	otherQualificationHash := digest(bytes.Replace(reservationSpecJSON(f), []byte(`"profile":`), []byte(`"qualification":"Other","profile":`), 1))
	matches := func(f *fleet.CelldFleet, res *fleet.CelldStorageReservation) bool {
		return (&Reconciler{}).reservationMatches(t.Context(), f, &loadedState{res: res}, fleetReservationSpec(f))
	}
	if !matches(f, res) {
		t.Fatal("v0.0.2 reservation rejected for its original fleet")
	}
	for name, change := range map[string]func(*fleet.CelldFleet, *fleet.CelldStorageReservation){
		"different qualification": func(_ *fleet.CelldFleet, r *fleet.CelldStorageReservation) {
			r.Spec.SpecHash = otherQualificationHash
		},
		"different fleet UID":        func(_ *fleet.CelldFleet, r *fleet.CelldStorageReservation) { r.Spec.FleetUID = "replacement" },
		"different bucket":           func(_ *fleet.CelldFleet, r *fleet.CelldStorageReservation) { r.Spec.Bucket = "other-bucket" },
		"different prefix":           func(_ *fleet.CelldFleet, r *fleet.CelldStorageReservation) { r.Spec.Prefix = "other" },
		"different initial replicas": func(_ *fleet.CelldFleet, r *fleet.CelldStorageReservation) { r.Spec.InitialReplicas++ },
		"changed immutable region":   func(f *fleet.CelldFleet, _ *fleet.CelldStorageReservation) { f.Spec.Storage.Region = "us-west-2" },
	} {
		t.Run(name, func(t *testing.T) {
			changedFleet := f.DeepCopy()
			changedRes := res.DeepCopy()
			change(changedFleet, changedRes)
			if matches(changedFleet, changedRes) {
				t.Fatal("changed reservation scope or immutable fleet spec accepted")
			}
		})
	}
}

func TestExportReservationCompat(t *testing.T) {
	f := fixture("export", "export-bucket", "Bucket")
	f.Spec.Export = &fleet.ExportSpec{Sink: "Kafka", Kafka: &fleet.ExportKafkaSpec{Brokers: []string{"kafka-0.kafka:9092"}, Egress: fleet.CollectorEgress{CIDR: "10.20.0.0/16"}}}
	without := f.DeepCopy()
	without.Spec.Export = nil
	if specHash(f) != specHash(without) {
		t.Fatal("export changed the reservation hash")
	}
	matches := func(f *fleet.CelldFleet, res *fleet.CelldStorageReservation) bool {
		return (&Reconciler{}).reservationMatches(t.Context(), f, &loadedState{res: res}, fleetReservationSpec(f))
	}
	// A fleet created without export, then given one, keeps its reservation.
	res := &fleet.CelldStorageReservation{Spec: fleetReservationSpec(without)}
	if !matches(f, res) {
		t.Fatal("adding export rejected the existing reservation")
	}
	// v0.0.8 and v0.0.9 hashed export into the reservation.
	res.Spec.SpecHash = legacyExportSpecHash(f)
	if res.Spec.SpecHash == specHash(f) {
		t.Fatal("legacy export hash must differ from the current hash")
	}
	if !matches(f, res) {
		t.Fatal("v0.0.9 export reservation rejected for its original fleet")
	}
	changed := f.DeepCopy()
	changed.Spec.Export.Kafka.Topic = "other"
	if matches(changed, res) {
		t.Fatal("v0.0.9 export reservation accepted a changed export")
	}
	if legacyExportSpecHash(without) != "" {
		t.Fatal("fleet without export has a legacy export hash")
	}
}
