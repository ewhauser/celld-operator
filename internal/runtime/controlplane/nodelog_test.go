package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func stateWith(t *testing.T, body string) State {
	t.Helper()
	s, err := decodeState([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	s.raw = []byte(body)
	return s
}

func TestNodeLogDecodesFleetView(t *testing.T) {
	s := stateWith(t, `{"node_log":{"posture":"fleet","session":"a-0/g1","own":{"state":"open","epoch":7,"ensemble":["a-1","a-2"],"bucket_complete":false,"active":true},"shipper_healthy":true,"fleet":{"observed_ms":1790000000000,"complete":true,"unrecovered":[{"session":"a-3/g0","state":"recovering","lease_expires_ms":1789999990000,"claimant":"a-1"}],"obligations":{"a-1":["a-0/g1"]}}}}`)
	got, err := s.NodeLog()
	if err != nil {
		t.Fatal(err)
	}
	if got.Posture != "fleet" || got.Session != "a-0/g1" || !got.ShipperHealthy || got.Own == nil || got.Own.Epoch != 7 || len(got.Own.Ensemble) != 2 || !got.Own.Active {
		t.Fatalf("own log %+v", got)
	}
	f := got.Fleet
	if f == nil || !f.Complete || !f.ObservedAt.Equal(time.UnixMilli(1790000000000)) || len(f.Unrecovered) != 1 || f.Unrecovered[0].Claimant != "a-1" || f.Obligations["a-1"][0] != "a-0/g1" {
		t.Fatalf("fleet view %+v", f)
	}
}

func TestNodeLogNullFields(t *testing.T) {
	s := stateWith(t, `{"node_log":{"posture":null,"session":"a-0/g1","own":null,"shipper_healthy":false,"fleet":null}}`)
	got, err := s.NodeLog()
	if err != nil || got.Posture != "" || got.Own != nil || got.Fleet != nil {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestNodeLogAbsentOrIncomplete(t *testing.T) {
	for _, body := range []string{`{}`, `{"node_log":null}`} {
		if _, err := stateWith(t, body).NodeLog(); !errors.Is(err, ErrNoNodeLog) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	for body, want := range map[string]string{
		`{"node_log":{"session":"a/g"}}`:                                   "incomplete node_log",
		`{"node_log":{"shipper_healthy":true}}`:                            "incomplete node_log",
		`{"node_log":{"session":"a/g","shipper_healthy":true,"fleet":{}}}`: "incomplete node_log fleet view",
		`{"node_log":{"session":"a/g","shipper_healthy":true,"fleet":{"observed_ms":1,"complete":true,"obligations":{},"unrecovered":[{}]}}}`: "without session",
		`{"node_log":{"session":1,"shipper_healthy":true}}`: "invalid node_log",
	} {
		if _, err := stateWith(t, body).NodeLog(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", body, err, want)
		}
	}
}
