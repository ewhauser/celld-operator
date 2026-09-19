package main

import (
	"fmt"
	"slices"
	"time"
)

// exerciseMaintenance covers live same-pin restart and RetainData deletion in
// the harness-owned cluster. It runs only after the identity, network and
// storage fixtures pass and never touches anything outside that cluster.
func (h *harness) exerciseMaintenance() {
	for _, f := range []struct{ fleet, kind, probe string }{
		{"alpha", "deployment", "client"},
		{"beta", "statefulset", "client-beta"},
	} {
		uid := uidOf(h.get("celldfleet", f.fleet))
		// The UID is captured once so the pod query still works after deletion.
		pods := func() []object { return h.listIn("fleets", "pods", "-l", "celld.eric.dev/fleet-uid="+uid) }
		h.merge(f.fleet, `{"spec":{"capacity":null,"replicas":3,"maintenance":null}}`)
		h.waitFor("maintenance fixture has three ready replicas: "+f.fleet, 360*time.Second, func() bool {
			return specReplicas(h.get(f.kind, f.fleet)) == 3 && h.ready(f.fleet) && len(sub(h.journal(f.fleet), "Operation")) == 0
		})
		assert(stored(h.app(f.probe, f.fleet, "PUT", "/?cell=maintenance&id=ack")), "maintenance write not acknowledged")
		before := podUIDs(pods())
		claims := map[string]string{}
		for _, claim := range h.listIn("fleets", "pvc", "-l", "celld.eric.dev/fleet-uid="+uid) {
			claims[nameOf(claim)] = uidOf(claim)
		}
		token := "live-maintenance-" + f.fleet
		h.merge(f.fleet, `{"spec":{"maintenance":{"restartToken":"`+token+`"}}}`)
		h.waitFor("restart irreversible authority persisted: "+f.fleet, 180*time.Second, func() bool {
			phase := str(h.journal(f.fleet), "Maintenance", "Phase")
			return phase == "Stopping" || phase == "Authorized" || phase == "Recovering"
		})
		h.restartOperator()
		h.waitFor("restart recovers across operator replacement: "+f.fleet, 600*time.Second, func() bool {
			return slices.Contains(strs(h.journal(f.fleet), "CompletedRestarts"), token) && h.ready(f.fleet)
		})
		after := podUIDs(pods())
		assert(len(after) == 3 && disjoint(before, after), "%v %v", before, after)
		assert(stored(h.app(f.probe, f.fleet, "GET", "/?cell=maintenance&id=ack")), "maintenance write unreadable after restart")
		for name, claimUID := range claims {
			assert(uidOf(h.get("pvc", name)) == claimUID, "claim %s was replaced by the restart", name)
		}
		h.restartOperator()
		h.sleep(12 * time.Second)
		assert(same(podUIDs(pods()), after), "completed token replayed")
		fmt.Println("PASS: exact-UID rolling restart, acknowledged data, retained claims, and token replay: " + f.fleet)

		retained := h.reservation(f.fleet)
		h.k("-n", "fleets", "delete", "celldfleet", f.fleet, "--wait=false")
		h.waitFor("RetainData finalizer completes with proven shutdown: "+f.fleet, 600*time.Second, func() bool {
			for _, value := range h.listIn("fleets", "celldfleets") {
				if uidOf(value) == uid {
					return false
				}
			}
			return true
		})
		assert(len(pods()) == 0, "runtime pods survived final shutdown")
		assert(uidOf(h.reservation(f.fleet)) == uidOf(retained), "reservation was replaced by final shutdown")
		assert(str(h.journal(f.fleet), "Maintenance", "Phase") == "Cleanup", "journal phase: %v", sub(h.journal(f.fleet), "Maintenance"))
		for name, claimUID := range claims {
			assert(uidOf(h.get("pvc", name)) == claimUID, "claim %s was replaced by final shutdown", name)
		}
		fmt.Println("PASS: final shutdown removes compute and preserves reservation/PVC identities: " + f.fleet)
	}
}

func disjoint(a, b map[string]bool) bool {
	for key := range a {
		if b[key] {
			return false
		}
	}
	return true
}
