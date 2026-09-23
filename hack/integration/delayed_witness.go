package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"
)

var (
	witnessRetry  = regexp.MustCompile(`celld predecessor recovery attempt [1-9][0-9]* failed: node-log recovery for ([^/\s]+)/([^:\s]+): no complete true witness among ([^\n]+) and [1-9][0-9]* member\(s\) undecided; refusing to seal while member state remains unverified`)
	witnessHealth = regexp.MustCompile(`(?m)^delayed_witness_http_status=(\d{3})$`)
)

func recoveryWaitObserved(logs, node, generation, witness string) bool {
	if node == "" || generation == "" || witness == "" {
		return false
	}
	for _, match := range witnessRetry.FindAllStringSubmatch(logs, -1) {
		if match[1] == node && match[2] == generation && strings.Contains(match[3], `"`+witness+`"`) {
			return true
		}
	}
	return false
}

// A failed kubectl exec is not evidence that an application is unready. Require
// curl's own result marker, including 000 for a connection that did not open.
func validateDelayedWitnessHealth(output string) error {
	match := witnessHealth.FindAllStringSubmatch(output, -1)
	if len(match) != 1 {
		return fmt.Errorf("delayed-witness health probe did not return one HTTP status: %s", output)
	}
	if match[0][1] != "000" && match[0][1] != "503" {
		return fmt.Errorf("recovering runtime must not serve health while its required witness is withheld: %s", output)
	}
	return nil
}

// delayPersistentWitness observes automatic recovery while this disposable
// test suspends a retained witness. A new child is not recovery evidence: require the
// exact predecessor's undecided-witness retry before starting the bounded hold.
func (h *harness) delayPersistentWitness(first, witness leaseLossTarget, storageUnchanged func() bool) {
	peerSuspended := func() {
		assert(storageUnchanged(), "delayed-witness recovery changed replicas, operation or disk identity")
		pod := h.get("pod", nameOf(witness.pod))
		assert(uidOf(pod) == uidOf(witness.pod) && celldRestarts(pod) == celldRestarts(witness.pod), "readiness failure restarted the suspended witness")
		state, err := h.launcherState(witness.fleet, pod)
		must(err)
		assert(state.Invocation == witness.state.Invocation && state.Generation == witness.state.Generation && state.PID == witness.state.PID && state.Phase == "Running" && !state.ChildExited && !state.RemovalReady(), "withheld witness child is no longer the exact suspended invocation")
	}
	var replacement object
	var child launcher.State
	h.waitFor("delayed witness: replacement beta-0 has a new exact launcher child", 3*time.Minute, func() bool {
		peerSuspended()
		pod, err := h.tryGet("fleets", "pod", nameOf(first.pod))
		if err != nil || uidOf(pod) != uidOf(first.pod) || str(pod, "status", "podIP") == "" {
			return false
		}
		state, err := h.launcherState(first.fleet, pod)
		if err != nil || state.PID == 0 || state.Generation == first.state.Generation {
			return false
		}
		assert(!state.ChildExited && state.Phase == "Running", "replacement child failed before delayed-witness observation: %v", state)
		assert(state.Generation != first.state.Generation && state.Invocation != first.state.Invocation, "replacement reused the old child identity")
		assert(state.BootID == first.state.BootID && state.DiskID == first.state.DiskID, "delayed-witness recovery changed boot or disk")
		replacement, child = pod, state
		return true
	})
	var logs string
	blocked := func() bool {
		peerSuspended()
		pod := h.get("pod", nameOf(replacement))
		assert(uidOf(pod) == uidOf(replacement), "recovering Pod was replaced during delayed-witness check")
		state, err := h.launcherState(first.fleet, pod)
		must(err)
		assert(state.Invocation == child.Invocation && state.Generation == child.Generation && state.PID == child.PID && !state.ChildExited && state.Phase == "Running", "recovering child changed during delayed-witness check")
		assert(state.Operation == "" && !state.RuntimeDataSafe() && !state.RemovalReady() && !state.InheritedLockReleased, "delayed-witness recovery manufactured removal authority")
		assert(!isReady(pod), "recovering Pod became Ready while its required witness was withheld")
		logs, err = h.try(command{args: h.kubectl("-n", "fleets", "logs", nameOf(pod), "-c", "celld", "--tail=-1", "--timestamps=true", "--limit-bytes=524288", "--request-timeout=5s"), timeout: 8 * time.Second})
		must(err)
		assert(len(logs) < 524288, "delayed-witness startup logs exceeded the observation bound")
		assert(!strings.Contains(logs, "declared bounded loss"), "runtime declared bounded loss while retained witness was withheld: %s", logs)
		assert(!strings.Contains(logs, "ready_gate_open"), "runtime opened readiness while retained witness was withheld: %s", logs)
		out, _ := h.try(command{args: h.kubectl("-n", "fleets", "exec", "client-beta", "-c", "probe", "--", "curl", "--silent", "--max-time", "1", "--output", "/dev/null", "--write-out", "\ndelayed_witness_http_status=%{http_code}\n", "http://"+str(pod, "status", "podIP")+":8080/.well-known/celld/health"), timeout: 5 * time.Second})
		must(validateDelayedWitnessHealth(out))
		return true
	}
	h.waitFor("delayed witness: exact predecessor recovery observes beta-1 unavailable", time.Minute, func() bool {
		blocked()
		return recoveryWaitObserved(logs, nameOf(first.pod), first.state.Generation, nameOf(witness.pod))
	})
	for line := range strings.SplitSeq(logs, "\n") {
		if recoveryWaitObserved(line, nameOf(first.pod), first.state.Generation, nameOf(witness.pod)) {
			fmt.Println("EVIDENCE: delayed witness:", line)
			break
		}
	}
	h.hold(20*time.Second, "delayed witness: recovery stays unready without loss or removal authority", blocked)
	assert(blocked(), "delayed-witness invariant changed before peer release")
	fmt.Printf("PASS: delayed witness releases peer=%s pod_uid=%s after exact child pod_uid=%s invocation=%s generation=%s pid=%d retried predecessor=%s/%s without readiness or loss\n", nameOf(witness.pod), uidOf(witness.pod), uidOf(replacement), child.Invocation, child.Generation, child.PID, nameOf(first.pod), first.state.Generation)
}
