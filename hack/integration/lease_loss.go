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
	type target struct {
		fleet string
		pod   object
		state launcher.State
		node  string
	}
	var targets []target
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
			targets = append(targets, target{fleetName, pod, state, uidOf(h.cluster("node", str(pod, "spec", "nodeName")))})
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
	failed := func() bool {
		assert(unchanged(), "lease loss changed replicas, Pod identity, operation or disk identity")
		for _, target := range targets {
			state, err := h.launcherState(target.fleet, target.pod)
			if err != nil || state.Phase != "ExitedUnrequested" || !state.ChildExited {
				return false
			}
			assert(state.Generation == target.state.Generation && state.Invocation == target.state.Invocation, "failed runtime invocation changed")
			assert(state.Operation == "" && !state.RemovalReady() && !state.RuntimeDataSafe() && !state.InheritedLockReleased, "lease loss manufactured removal authority")
		}
		return true
	}
	// Include the acknowledged tail immediately before S3 becomes unavailable,
	// not only values which had the entire earlier fault sequence to upload.
	h.writeLedger("client", "alpha")
	h.writeLedger("client-beta", "beta")
	h.toxic("POST", "/proxies/minio/toxics", object{"name": "partition", "type": "timeout", "stream": "upstream", "attributes": object{"timeout": 0}})
	func() {
		defer h.toxic("DELETE", "/proxies/minio/toxics/partition", nil)
		// Native node leases expire after ten seconds. Wait for explicit process
		// evidence, not delayed Kubernetes readiness projections.
		h.waitFor("S3 lease expiry stops every runtime without removal authority", time.Minute, failed)
	}()
	for _, target := range targets {
		// Preserve predecessor recovery and acknowledged-tail evidence before
		// ordinary Pod replacement makes these container logs unavailable.
		const limit = 8 << 20
		fmt.Printf("BEGIN PRE-REPLACEMENT RUNTIME LOG pod=%s uid=%s limit_bytes=%d\n", nameOf(target.pod), uidOf(target.pod), limit)
		logs, err := h.try(command{args: h.kubectl("-n", "fleets", "logs", nameOf(target.pod), "-c", "celld", "--tail=-1", "--timestamps=true", "--limit-bytes=8388608", "--request-timeout=15s"), timeout: 20 * time.Second})
		fmt.Printf("%s\n", logs)
		if len(logs) >= limit {
			fmt.Println("PRE-REPLACEMENT RUNTIME LOG BYTE LIMIT REACHED; capture may be truncated")
		}
		fmt.Printf("END PRE-REPLACEMENT RUNTIME LOG pod=%s uid=%s\n", nameOf(target.pod), uidOf(target.pod))
		must(err)
		assert(strings.Contains(logs, "node_lease_watchdog_fence"), "%s did not stop because its S3 lease expired", nameOf(target.pod))
	}
	h.hold(12*time.Second, "restoring S3 does not restart failed children or delete disks", failed)
	fmt.Println("PASS: lease-loss fail-stop preserves all Pod/PVC/PV identities; no strict proof or automatic restart")

	// This is an explicit administrative action in the disposable test. It is
	// ordinary same-host recovery of nonretired storage with no active operation,
	// never a substitute for a strict retirement proof or cross-host fencing.
	for _, target := range targets {
		assert(len(sub(h.currentState(target.fleet), "Operation")) == 0, "administrative recovery raced a lifecycle operation")
		state, err := h.launcherState(target.fleet, target.pod)
		must(err)
		assert(state.Phase == "ExitedUnrequested" && state.ChildExited && !state.RemovalReady(), "refusing to replace a runtime that has not failed")
		body := encode(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": uidOf(target.pod)}})
		path := h.writeFile("delete-failed-"+nameOf(target.pod)+".json", []byte(body))
		h.k("delete", "--raw", "/api/v1/namespaces/fleets/pods/"+nameOf(target.pod), "-f", path)
	}
	h.waitFor("explicit administrative Pod replacement restores both fleets", 8*time.Minute, func() bool {
		for _, target := range targets {
			var pod object
			for _, candidate := range h.fleetPods(target.fleet) {
				if nameOf(candidate) == nameOf(target.pod) {
					pod = candidate
				}
			}
			if pod == nil || uidOf(pod) == uidOf(target.pod) || str(pod, "status", "podIP") == "" {
				return false
			}
			state, err := h.launcherState(target.fleet, pod)
			if err != nil || state.Phase != "Running" || state.Generation == target.state.Generation || state.Invocation == target.state.Invocation {
				return false
			}
			if target.fleet == "beta" {
				assert(str(pod, "status", "podIP") != str(target.pod, "status", "podIP"), "PersistentFleet reused its Pod IP; DNS rebinding was not exercised")
				assert(str(pod, "spec", "nodeName") == str(target.pod, "spec", "nodeName") && uidOf(h.cluster("node", str(pod, "spec", "nodeName"))) == target.node, "persistent recovery moved to another node identity")
				assert(state.BootID == target.state.BootID && state.DiskID == target.state.DiskID, "persistent recovery changed boot or disk identity")
			}
		}
		return h.settled("alpha", 2) && h.settled("beta", 2)
	})
	for fleetName, before := range oldClaims {
		assert(same(h.claims(fleetName), before), "administrative recovery replaced claims or CSI disks")
	}
	for _, target := range targets {
		if target.fleet != "beta" {
			continue
		}
		pod := h.get("pod", nameOf(target.pod))
		advertise := strings.TrimSpace(h.k("-n", "fleets", "exec", nameOf(pod), "-c", "celld", "--", "printenv", "CELLD_ADVERTISE"))
		want := fmt.Sprintf("%s.%s-peers.%s.svc:8081", nameOf(pod), target.fleet, str(pod, "metadata", "namespace"))
		assert(advertise == want, "PersistentFleet advertises %q, expected stable peer DNS %q", advertise, want)
		fmt.Printf("PASS: PersistentFleet DNS rebinding pod=%s old_ip=%s new_ip=%s advertise=%s\n", nameOf(pod), str(target.pod, "status", "podIP"), str(pod, "status", "podIP"), advertise)
	}
	h.readLedger("client", "alpha")
	h.readLedger("client-beta", "beta")
	fmt.Println("PASS: explicit administrative recovery uses new Pod/runtime identities and retains same-host PersistentFleet disks and every acknowledged write")
}
