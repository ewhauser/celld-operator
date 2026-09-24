package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"
)

const launcherInspector = "launcher-inspector"

type leaseLossTarget struct {
	fleet string
	pod   object
	state launcher.State
	node  string
}

// launcherState makes only authenticated observations, never stop requests.
// The probe is created after the crash tests and removed before maintenance.
func (h *harness) launcherState(fleetName string, pod object) (launcher.State, error) {
	var answer launcher.Response
	key, err := base64.StdEncoding.DecodeString(str(h.get("secret", fleetName+"-launcher"), "data", "key"))
	if err != nil || len(key) != 32 {
		return answer.State, fmt.Errorf("invalid launcher observation key for %s", fleetName)
	}
	req := launcher.Request{Nonce: launcher.Nonce()}
	out, err := h.tryK("-n", operatorNS, "exec", launcherInspector, "-c", "probe", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "3", "-X", "POST",
		"-H", "Content-Type: application/json", "-H", "X-Celld-MAC: "+launcher.MAC(key, "request", req), "-d", encode(req),
		"http://"+str(pod, "status", "podIP")+":8083/v2")
	if err != nil {
		return answer.State, err
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return answer.State, err
	}
	signed := struct {
		Nonce string
		State launcher.State
	}{answer.Nonce, answer.State}
	if answer.Nonce != req.Nonce || !launcher.Verify(key, "response", signed, answer.MAC) || answer.State.PodUID != uidOf(pod) || answer.State.Host != str(pod, "spec", "nodeName") || answer.State.Generation == "" || answer.State.Invocation == "" {
		return launcher.State{}, fmt.Errorf("launcher observation does not bind to %s", nameOf(pod))
	}
	return answer.State, nil
}

func (h *harness) exerciseLeaseLoss() {
	h.probe(launcherInspector, operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	defer h.k("-n", operatorNS, "delete", "pod", launcherInspector, "--wait=true")
	var targets []leaseLossTarget
	oldClaims := map[string]map[string]claimIdentity{}
	oldPods := map[string]map[string]string{}
	for _, fleetName := range []string{"alpha", "beta"} {
		assert(h.settled(fleetName, 2), "lease-loss test requires an idle fleet")
		oldClaims[fleetName] = h.claims(fleetName)
		pods := h.fleetPods(fleetName)
		oldPods[fleetName] = nameUIDs(pods)
		for _, pod := range pods {
			state, err := h.launcherState(fleetName, pod)
			must(err)
			assert(state.Phase == "Running", "lease-loss target is not running")
			targets = append(targets, leaseLossTarget{fleetName, pod, state, uidOf(h.cluster("node", str(pod, "spec", "nodeName")))})
		}
	}
	unchanged := func() bool {
		for _, fleetName := range []string{"alpha", "beta"} {
			if specReplicas(h.get("statefulset", fleetName)) != 2 || len(sub(h.currentState(fleetName), "Operation")) != 0 || !same(nameUIDs(h.fleetPods(fleetName)), oldPods[fleetName]) || !same(h.claims(fleetName), oldClaims[fleetName]) {
				return false
			}
		}
		return true
	}
	var recovering, witness *leaseLossTarget
	for i := range targets {
		target := &targets[i]
		if nameOf(target.pod) == "beta-0" {
			recovering = target
		}
		if nameOf(target.pod) == "beta-1" {
			witness = target
		}
	}
	assert(recovering != nil && witness != nil, "delayed-witness recovery requires both persistent ordinals")
	// Include the acknowledged tail immediately before S3 becomes unavailable,
	// not only values which had the entire earlier fault sequence to upload.
	h.writeLedger("client", "alpha")
	h.writeLedger("client-beta", "beta")
	// Suspend the retained witness without exiting it. Its launcher stays live
	// despite failed application readiness; beta-0 must wait for this exact
	// predecessor instead of declaring loss or restarting during recovery.
	h.k("-n", "fleets", "exec", nameOf(witness.pod), "-c", "celld", "--", "/bin/sh", "-c", `kill -STOP "$1"`, "signal-child", fmt.Sprint(witness.state.PID))
	resumeWitness := func() {
		h.k("-n", "fleets", "exec", nameOf(witness.pod), "-c", "celld", "--", "/bin/sh", "-c", `kill -CONT "$1"`, "signal-child", fmt.Sprint(witness.state.PID))
	}
	witnessReleased := false
	defer func() {
		if !witnessReleased {
			resumeWitness()
		}
	}()
	fenced := map[string]bool{}
	observeFence := func(target leaseLossTarget) bool {
		name := nameOf(target.pod)
		if fenced[name] {
			return true
		}
		// A probe may already have restarted PID 1. Accept fence evidence from
		// either container log, never from readiness or a missing process alone.
		for _, previous := range []bool{false, true} {
			logs, err := h.tryK("-n", "fleets", "logs", name, "-c", "celld", fmt.Sprintf("--previous=%t", previous), "--tail=-1", "--limit-bytes=8388608", "--request-timeout=5s")
			if err == nil && strings.Contains(logs, "node_lease_watchdog_fence") {
				fenced[name] = true
				fmt.Printf("PASS: lease watchdog self-fenced pod=%s uid=%s previous=%t\n", name, uidOf(target.pod), previous)
				return true
			}
		}
		return false
	}
	h.toxic("POST", "/proxies/minio/toxics", object{"name": "partition", "type": "timeout", "stream": "upstream", "attributes": object{"timeout": 0}})
	func() {
		defer h.toxic("DELETE", "/proxies/minio/toxics/partition", nil)
		// Native node leases expire after ten seconds. The suspended witness
		// cannot run its watchdog until released below.
		h.waitFor("S3 lease expiry self-fences all runnable children", 2*time.Minute, func() bool {
			assert(unchanged(), "lease loss changed replicas, Pod identity, operation or disk identity")
			all := true
			for _, target := range targets {
				if nameOf(target.pod) != nameOf(witness.pod) && !observeFence(target) {
					all = false
				}
			}
			return all
		})
	}()
	h.delayPersistentWitness(*recovering, *witness, unchanged)
	// Also cover an arbitrary child crash. The other three targets exercised
	// watchdog exits; killing this suspended child makes witness release
	// deterministic instead of racing lease renewal against its watchdog.
	h.k("-n", "fleets", "exec", nameOf(witness.pod), "-c", "celld", "--", "/bin/sh", "-c", `kill -KILL "$1"`, "signal-child", fmt.Sprint(witness.state.PID))
	witnessReleased = true
	h.waitFor("kubelet automatically restores both fleets on the same Pods and disks", 8*time.Minute, func() bool {
		for _, target := range targets {
			var pod object
			for _, candidate := range h.fleetPods(target.fleet) {
				if nameOf(candidate) == nameOf(target.pod) {
					pod = candidate
				}
			}
			assert(unchanged(), "automatic recovery replaced Pods or storage, or created a removal operation")
			if pod == nil || str(pod, "status", "podIP") == "" || celldRestarts(pod) <= celldRestarts(target.pod) {
				return false
			}
			state, err := h.launcherState(target.fleet, pod)
			if err != nil || state.Phase != "Running" || state.Generation == target.state.Generation || state.Invocation == target.state.Invocation {
				return false
			}
			assert(state.Operation == "" && !state.ChildExited && !state.RemovalReady() && !state.RuntimeDataSafe() && !state.InheritedLockReleased && !state.RestartDenied, "recovery manufactured removal authority")
			if target.fleet == "beta" {
				assert(str(pod, "spec", "nodeName") == str(target.pod, "spec", "nodeName") && uidOf(h.cluster("node", str(pod, "spec", "nodeName"))) == target.node, "persistent recovery moved to another node identity")
				assert(state.BootID == target.state.BootID && state.DiskID == target.state.DiskID, "persistent recovery changed boot or disk identity")
			}
		}
		return h.settled("alpha", 2) && h.settled("beta", 2)
	})
	for fleetName, before := range oldClaims {
		assert(same(h.claims(fleetName), before), "automatic recovery replaced claims or CSI disks")
	}
	for _, target := range targets {
		if target.fleet != "beta" {
			continue
		}
		pod := h.get("pod", nameOf(target.pod))
		advertise := strings.TrimSpace(h.k("-n", "fleets", "exec", nameOf(pod), "-c", "celld", "--", "printenv", "CELLD_ADVERTISE"))
		want := fmt.Sprintf("%s.%s-peers.%s.svc:8081", nameOf(pod), target.fleet, str(pod, "metadata", "namespace"))
		assert(advertise == want, "PersistentFleet advertises %q, expected stable peer DNS %q", advertise, want)
		fmt.Printf("PASS: PersistentFleet same-Pod recovery pod=%s old_ip=%s new_ip=%s advertise=%s\n", nameOf(pod), str(target.pod, "status", "podIP"), str(pod, "status", "podIP"), advertise)
	}
	h.readLedger("client", "alpha")
	h.readLedger("client-beta", "beta")
	fmt.Println("PASS: automatic container recovery uses fresh runtime identities, retains Pod/storage identities and every acknowledged write")
}

func celldRestarts(pod object) int64 {
	for _, container := range list(pod, "status", "containerStatuses") {
		if str(container, "name") == "celld" {
			return num(container, "restartCount")
		}
	}
	return 0
}
