package main

import (
	"fmt"
	"strings"
	"time"
)

func (h *harness) exerciseFaults() {
	for _, f := range []struct{ name, probe string }{{"alpha", "client"}, {"beta", "client-beta"}} {
		h.writeLedger(f.probe, f.name)
		h.scale(f.name, 3)
		for _, point := range []string{"before-effect", "after-effect"} {
			before := generation(h.get("statefulset", f.name))
			h.setOperatorFault(point)
			h.setReplicas(f.name, 2)
			h.waitFor("manager crashes at "+point+": "+f.name, 8*time.Minute, func() bool {
				out, _ := h.tryK("-n", operatorNS, "logs", "deployment/celld-operator", "--previous")
				return strings.Contains(out, "injected crash at lifecycle fault point \""+point+"\"")
			})
			state := h.currentState(f.name)
			op := sub(state, "Operation")
			assert(str(op, "ID") != "" && str(op, "Phase") == "Apply", "no durable apply authority at crash: %v", op)
			for _, target := range list(op, "Targets") {
				assert(sub(target, "Proof") != nil, "workload effect without captured strict proof")
			}
			expected := int64(3)
			expectedGen := before
			if point == "after-effect" {
				expected = 2
				expectedGen++
			}
			assert(specReplicas(h.get("statefulset", f.name)) == expected, "unexpected workload effect")
			h.setOperatorFault("")
			h.waitFor("manager resumes exact operation: "+f.name, 8*time.Minute, func() bool { return h.settled(f.name, 2) })
			completion := sub(h.currentState(f.name), "Completion")
			assert(str(completion, "ID") == str(op, "ID"), "manager completed a different operation")
			assert(generation(h.get("statefulset", f.name)) == before+1, "more than one workload replica write: before %d, crashed %d", before, expectedGen)
			h.readLedger(f.probe, f.name)
			h.scale(f.name, 3)
		}
		h.scale(f.name, 2)
	}
	// This is a storage transport fault, not private object decoding. Runtime state
	// remains the authority and the harness only checks count, identity and data.
	h.toxic("POST", "/proxies/minio/toxics", object{"name": "latency", "type": "latency", "stream": "downstream", "attributes": object{"latency": 250, "jitter": 50}})
	func() {
		defer h.toxic("DELETE", "/proxies/minio/toxics/latency", nil)
		h.scale("alpha", 3)
		h.scale("alpha", 2)
		h.readLedger("client", "alpha")
	}()
	h.toxic("POST", "/proxies/minio/toxics", object{"name": "partition", "type": "timeout", "stream": "upstream", "attributes": object{"timeout": 0}})
	func() {
		defer h.toxic("DELETE", "/proxies/minio/toxics/partition", nil)
		h.hold(10*time.Second, "steady-state storage outage creates no operation or disk deletion", func() bool {
			return specReplicas(h.get("statefulset", "alpha")) == 2 && specReplicas(h.get("statefulset", "beta")) == 2 && len(sub(h.currentState("alpha"), "Operation")) == 0 && len(sub(h.currentState("beta"), "Operation")) == 0
		})
	}()
	h.waitFor("fleets observable after storage transport restoration", 5*time.Minute, func() bool { return h.ready("alpha") && h.ready("beta") })
	h.readLedger("client", "alpha")
	h.readLedger("client-beta", "beta")
	fmt.Println("PASS: both profiles preserve acknowledged writes across before/after-effect manager crashes; storage latency and short outage")
}

func (h *harness) toxic(method, path string, body any) {
	args := []string{"-n", storeNS, "exec", "toxi-ctl", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "5", "-X", method, "http://toxiproxy-api:8474" + path}
	if body != nil {
		args = append(args, "-H", "Content-Type: application/json", "-d", encode(body))
	}
	h.k(args...)
}
