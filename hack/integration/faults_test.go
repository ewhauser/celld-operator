package main

import (
	"strings"
	"testing"
)

func TestAcksRecordOnlyExactAcknowledgements(t *testing.T) {
	logs := strings.Join([]string{
		"ACK load-1 w12-1",
		"ACK load-2 w12-2 trailing",
		"curl: (28) Operation timed out",
		"ACK load-x w12-3",
		"  ACK load-4 w12-4  ",
		"STOPPED",
	}, "\n")
	got := acks(logs)
	want := []ledgerEntry{{"load-1", "w12-1"}, {"load-4", "w12-4"}}
	if !same(got, want) {
		t.Fatalf("acks = %v; want %v", got, want)
	}
}

func TestVerifiedReadsRequiresEveryAcknowledgedWrite(t *testing.T) {
	if err := verifiedReads("VERIFIED 3\n", 3); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"missing entry":   "MISSING load-1 w1-1 {\"id\":\"w1-1\",\"stored\":false}\nVERIFIED 3\n",
		"short count":     "VERIFIED 2\n",
		"no summary":      "error: unable to upgrade connection\n",
		"exec cut short":  "",
		"excess coverage": "VERIFIED 4\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifiedReads(out, 3); err == nil {
				t.Fatalf("incomplete read check accepted: %q", out)
			}
		})
	}
}

func readyPod(name string, ready bool) object {
	status := "False"
	if ready {
		status = "True"
	}
	return object{"metadata": object{"name": name}, "status": object{"conditions": []any{object{"type": "Ready", "status": status}}}}
}

func TestDownMembersCountsMissingUnreadyAndTerminating(t *testing.T) {
	terminatingPod := readyPod("beta-2", true)
	terminatingPod["metadata"].(object)["deletionTimestamp"] = "2026-09-25T00:00:00Z"
	pods := map[string]object{
		"beta-0": readyPod("beta-0", true),
		"beta-1": readyPod("beta-1", false),
		"beta-2": terminatingPod,
		"beta-4": readyPod("beta-4", false),
	}
	if got := downMembers("beta", 4, pods); !same(got, []string{"beta-1", "beta-2", "beta-3"}) {
		t.Fatalf("down = %v", got)
	}
	// Ordinals at or above replicas are being removed, not disrupted.
	if got := downMembers("beta", 1, pods); len(got) != 0 {
		t.Fatalf("down = %v", got)
	}
}

func TestSameDisksRejectsAnyIdentityChange(t *testing.T) {
	before := map[string]claimIdentity{"data-beta-0": {"c0", "pv0", "u0", "h0"}, "data-beta-1": {"c1", "pv1", "u1", "h1"}}
	if err := sameDisks(before, map[string]claimIdentity{"data-beta-0": before["data-beta-0"], "data-beta-1": before["data-beta-1"]}); err != nil {
		t.Fatal(err)
	}
	changed := map[string]claimIdentity{"data-beta-0": before["data-beta-0"], "data-beta-1": {"c1", "pv1", "u1", "h2"}}
	if err := sameDisks(before, changed); err == nil {
		t.Fatal("changed CSI handle accepted")
	}
	if err := sameDisks(before, map[string]claimIdentity{"data-beta-0": before["data-beta-0"]}); err == nil {
		t.Fatal("missing claim accepted")
	}
	if !freshDisk(before["data-beta-0"], claimIdentity{"c9", "pv9", "u9", "h9"}) || freshDisk(before["data-beta-0"], claimIdentity{"c9", "pv9", "u0", "h9"}) {
		t.Fatal("freshDisk must require new claim, volume and handle identities")
	}
}

func TestClaimOrdinal(t *testing.T) {
	for claim, want := range map[string]int{"data-beta-0": 0, "data-beta-12": 12} {
		if got, ok := claimOrdinal("beta", claim); !ok || got != want {
			t.Fatalf("%s = %d, %v", claim, got, ok)
		}
	}
	for _, claim := range []string{"data-beta-01", "data-beta-x", "data-betamax-1", "data-collision-0", "beta-1"} {
		if _, ok := claimOrdinal("beta", claim); ok {
			t.Fatalf("%s accepted", claim)
		}
	}
}
