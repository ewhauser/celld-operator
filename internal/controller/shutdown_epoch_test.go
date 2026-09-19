package controller

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	v050 "github.com/ewhauser/celld-operator/internal/runtime/v050"
)

type shutdownEpochReader struct {
	*persistentReader
	epoch    uint64
	unsealed bool
}

func (r *shutdownEpochReader) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := r.persistentReader.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, err
	}
	log := record["log"].(map[string]any)
	log["epoch"] = r.epoch
	log["state"] = "sealed"
	if r.unsealed {
		log["state"] = "open"
	}
	record["expires_ms"] = r.now.Add(-time.Second).UnixMilli()
	return json.Marshal(record)
}

func shutdownEpochSetup(t *testing.T) (*persistentFixture, []persistentMember, *shutdownEpochReader) {
	t.Helper()
	p := persistentSetup(t)
	members, err := p.r.persistentMembers(t.Context(), p.f, p.j, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range members {
		members[i].Epoch = 1
		members[i].Stopped = true
		members[i].RestartDenied = true
	}
	reader := &shutdownEpochReader{persistentReader: p.reader, epoch: 1}
	p.r.Evidence.reader = func(context.Context, *fleet.CelldFleet) (v050.Reader, error) { return reader, nil }
	return p, members, reader
}

func TestShutdownRejectsEveryPriorGenerationEpoch(t *testing.T) {
	for _, source := range []string{"capture", "session", "history", "retired-history", "maintenance"} {
		t.Run(source, func(t *testing.T) {
			p, members, _ := shutdownEpochSetup(t)
			switch source {
			case "capture":
				members[0].Epoch = 2
			case "session":
				p.j.Inventory.Sessions = append(p.j.Inventory.Sessions, RuntimeSession{Node: members[0].Node, Generation: members[0].Generation, Epoch: 2})
			case "history":
				old := members[0]
				old.Epoch = 2
				p.j.PersistentHistory = []persistentMember{old}
			case "retired-history":
				old := members[2]
				old.Epoch = 2
				old.Retired = true
				p.j.PersistentHistory = []persistentMember{old}
				members = members[:2]
				p.j.Applied = 2
			case "maintenance":
				old := members[0]
				old.Epoch = 2
				p.j.Maintenance = &maintenanceOperation{Persistent: []persistentMember{old}}
			}
			if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
				t.Fatal("older sealed epoch accepted")
			}
		})
	}
}

func TestShutdownPersistsLateEpochBeforeBlockedRecovery(t *testing.T) {
	p, members, reader := shutdownEpochSetup(t)
	reader.epoch = 2
	reader.unsealed = true
	if _, err := p.r.shutdownInventory(t.Context(), p.f, p.j, members, true); err == nil {
		t.Fatal("unsealed log accepted")
	}
	if members[0].Epoch != 2 || !slices.ContainsFunc(p.j.Inventory.Sessions, func(s RuntimeSession) bool {
		return s.Node == members[0].Node && s.Generation == members[0].Generation && s.Epoch == 2
	}) {
		t.Fatal("late epoch was not retained on blocker")
	}
	if err := p.r.saveJournal(t.Context(), p.res, p.j); err != nil {
		t.Fatal(err)
	}
	restored, err := readJournal(p.res)
	if err != nil {
		t.Fatal(err)
	}
	reader.epoch = 1
	reader.unsealed = false
	// Simulate retry after a leader crash with the original capture still at E1.
	for i := range members {
		members[i].Epoch = 1
	}
	if _, err := p.r.shutdownInventory(t.Context(), p.f, restored, members, true); err == nil {
		t.Fatal("journal replay lost newer observed epoch")
	}
	reader.epoch = 2
	if _, err := p.r.shutdownInventory(t.Context(), p.f, restored, members, true); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Epoch != 2 {
			t.Fatal("successful retirement did not retain final epoch")
		}
	}
}
