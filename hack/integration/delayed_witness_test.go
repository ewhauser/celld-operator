package main

import (
	"strings"
	"testing"
)

func TestRecoveryWaitBindsExactPredecessorAndWitness(t *testing.T) {
	line := `2026-09-21T00:03:47Z celld predecessor recovery attempt 1 failed: node-log recovery for beta-0/old-generation: no complete true witness among ["beta-1"] and 1 member(s) undecided; refusing to seal while member state remains unverified`
	if !recoveryWaitObserved(line, "beta-0", "old-generation", "beta-1") {
		t.Fatal("exact unavailable-witness retry was not recognized")
	}
	for name, input := range map[string]string{
		"wrong recovering node": strings.ReplaceAll(line, "beta-0/", "beta-1/"),
		"wrong generation":      strings.ReplaceAll(line, "old-generation:", "old-generation-other:"),
		"wrong witness":         strings.ReplaceAll(line, `"beta-1"`, `"beta-10"`),
		"no undecided member":   strings.ReplaceAll(line, "1 member(s)", "0 member(s)"),
		"no actual retry":       strings.ReplaceAll(line, "celld predecessor recovery attempt 1 failed: ", ""),
		"unrelated lines":       strings.ReplaceAll(line, ": no complete true witness", ":\nno complete true witness"),
		"start only":            `recovering a predecessor session's node log session="beta-0/old-generation"`,
	} {
		t.Run(name, func(t *testing.T) {
			if recoveryWaitObserved(input, "beta-0", "old-generation", "beta-1") {
				t.Fatal("unrelated or incomplete evidence accepted")
			}
		})
	}
}

func TestDelayedWitnessHealthRequiresActualProbeAndRejectsReadiness(t *testing.T) {
	for _, output := range []string{"delayed_witness_http_status=000\ncommand terminated with exit code 7", "delayed_witness_http_status=503\n"} {
		if err := validateDelayedWitnessHealth(output); err != nil {
			t.Fatal(err)
		}
	}
	for _, output := range []string{"delayed_witness_http_status=200\n", "delayed_witness_http_status=204\n", "pod not found", "error: unable to upgrade connection", "delayed_witness_http_status=503\ndelayed_witness_http_status=200\n"} {
		if err := validateDelayedWitnessHealth(output); err == nil {
			t.Fatalf("healthy or unobserved runtime accepted: %q", output)
		}
	}
}
