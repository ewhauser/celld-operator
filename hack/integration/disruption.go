package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/internal/launcher"
)

// exerciseDisruption deletes a running PersistentFleet Pod without any operator
// request, as kubectl delete, node-pressure eviction, node shutdown or
// preemption would, and requires the fleet to return to service on its own.
// It records the replacement launcher's authenticated state and the disk's
// retirement marker so a failure explains itself (issue #60).
func (h *harness) exerciseDisruption() {
	const fleetName, target = "beta", "beta-0"
	h.probe(launcherInspector, operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	defer h.k("-n", operatorNS, "delete", "pod", launcherInspector, "--wait=true")
	h.writeLedger("client-beta", fleetName)
	old := h.get("pod", target)
	state, err := h.observeLauncher(fleetName, old)
	must(err)
	assert(state.Phase == "Running" && state.PID > 0 && !state.ChildExited, "disruption target is not a running child: %+v", state)
	claims := h.claims(fleetName)
	claim, ok := claims["data-"+target]
	assert(ok, "no claim for %s", target)
	fmt.Printf("Disruption target %s uid=%s node=%s phase=%s pid=%d claim=%s volume=%s\n", target, uidOf(old), str(old, "spec", "nodeName"), state.Phase, state.PID, claim.UID, claim.Volume)

	// Ordinary grace, no UID precondition bypass and no operator request.
	h.k("-n", "fleets", "delete", "pod", target, "--wait=true")
	var replacement object
	h.waitFor("StatefulSet recreates "+target, 3*time.Minute, func() bool {
		pod, err := h.tryGet("fleets", "pod", target)
		if err != nil || uidOf(pod) == uidOf(old) || str(pod, "status", "podIP") == "" {
			return false
		}
		replacement = pod
		return true
	})
	current := h.claims(fleetName)["data-"+target]
	assert(current.UID == claim.UID && current.VolumeUID == claim.VolumeUID, "replacement did not retain the original claim; this scenario needs the same disk")
	fmt.Printf("Replacement %s uid=%s node=%s claim=%s (retained)\n", target, uidOf(replacement), str(replacement, "spec", "nodeName"), current.UID)

	recovered := false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) && !recovered {
		pod, err := h.tryGet("fleets", "pod", target)
		if err == nil {
			s, lerr := h.observeLauncher(fleetName, pod)
			f := h.get("celldfleet", fleetName)
			ready, blocked := condition(f, "Ready"), condition(f, "Blocked")
			fmt.Printf("  observe %s: podReady=%v celldRestarts=%d launcher=%s pid=%d error=%q observeErr=%v | fleet Ready=%s/%s Blocked=%s/%s %q | operation=%v\n",
				time.Now().Format(time.TimeOnly), podIsReady(pod), celldRestarts(pod), s.Phase, s.PID, s.Error, lerr,
				str(ready, "status"), str(ready, "reason"), str(blocked, "status"), str(blocked, "reason"), truncate(str(blocked, "message"), 160),
				len(sub(h.currentState(fleetName), "Operation")) != 0)
			recovered = podIsReady(pod) && s.Phase == "Running" && s.PID > 0 && h.ready(fleetName)
		}
		if !recovered {
			h.sleep(15 * time.Second)
		}
	}
	node := str(replacement, "spec", "nodeName")
	markers, _ := h.try(command{args: []string{"docker", "exec", node, "sh", "-c", "find / -xdev -name .celld-launcher-retired -not -path '/proc/*' 2>/dev/null | head -20"}, timeout: time.Minute})
	fmt.Printf("Retirement markers on %s:\n%s\n", node, markers)
	readable := 0
	for _, entry := range h.ledgers[fleetName] {
		// Non-asserting: availability after the disruption is what is being measured.
		out, err := h.tryK("-n", "fleets", "exec", "client-beta", "--", "curl", "--fail", "--silent", "--max-time", "3", "http://"+fleetName+":8080"+entry.path)
		if err != nil {
			continue
		}
		var response object
		if json.Unmarshal([]byte(out), &response) == nil && str(response, "id") == entry.id && field(response, "stored") == true {
			readable++
		}
	}
	fmt.Printf("Acknowledged writes readable through the fleet Service: %d/%d\n", readable, len(h.ledgers[fleetName]))
	assert(recovered, "PersistentFleet did not return to service within 5m after an unrequested deletion of running Pod %s", target)
	fmt.Println("PASS: PersistentFleet recovered from an unrequested Pod deletion on its retained disk")
}

// observeLauncher is launcherState without requiring a running generation, so
// a Blocked launcher on a retired disk can still be read. It remains an
// authenticated, read-only observation bound to the exact Pod UID.
func (h *harness) observeLauncher(fleetName string, pod object) (launcher.State, error) {
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
	if answer.Nonce != req.Nonce || !launcher.Verify(key, "response", signed, answer.MAC) || answer.State.PodUID != uidOf(pod) {
		return launcher.State{}, fmt.Errorf("launcher observation does not bind to %s", nameOf(pod))
	}
	return answer.State, nil
}

func podIsReady(pod object) bool {
	for _, c := range conditions(pod) {
		if str(c, "type") == "Ready" {
			return strings.EqualFold(str(c, "status"), "True")
		}
	}
	return false
}
