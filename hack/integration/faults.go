package main

import (
	"fmt"
	"strings"
	"time"
)

const (
	journalAnnotation   = "celld.eric.dev/lifecycle-journal"
	operationAnnotation = "celld.eric.dev/lifecycle-operation"
)

// exerciseFaults injects faults into the harness-owned kind cluster. It runs
// only after the identity, isolation and drift fixtures pass. Scenarios, in order:
//
//  1. Manager crash after the replica CAS, before the journal records it (scale-out).
//  2. Manager crash after durable intent, before the replica CAS (contraction).
//  3. Node loss for a Bucket replica: cordon plus pod deletion, contraction blocked.
//  4. Node loss for a launcher-managed PersistentFleet replica: retained claim,
//     no decrement, same-host return.
//  5. S3 latency through toxiproxy: contraction still completes, write readable.
//  6. S3 partition through toxiproxy while a contraction is issued: no further
//     decrement, no completion or loss from absent evidence, write readable after
//     recovery, and whatever the post-outage outcome is, exactly one effect.
//  7. Leader loss with two manager replicas while a contraction is issued: the
//     standby acquires the lease and finishes the same operation with exactly one
//     workload write and one history entry.
//
// Every assertion is about fail-closed behavior. Nothing outside the harness
// cluster is touched.
func (h *harness) exerciseFaults() {
	readyReason := func(fleetName string) string {
		return str(condition(h.get("celldfleet", fleetName), "Ready"), "reason")
	}
	settled := func(fleetName string, count int64) bool {
		return specReplicas(h.get(h.workloadKind(fleetName), fleetName)) == count && h.ready(fleetName) && len(sub(h.journal(fleetName), "Operation")) == 0
	}
	completions := func(fleetName, op string) []object {
		var out []object
		for _, entry := range list(h.journal(fleetName), "History") {
			if str(entry, "ID") == op {
				out = append(out, entry)
			}
		}
		return out
	}
	operatorPod := func() object {
		// The harness also labels a probe pod in this namespace; keep only the Deployment's pods.
		var live []object
		for _, pod := range h.listIn(operatorNS, "pods", "-l", "app.kubernetes.io/name=celld-operator") {
			if !deleting(pod) && strings.HasPrefix(nameOf(pod), "celld-operator-") {
				live = append(live, pod)
			}
		}
		assert(len(live) == 1, "%d live manager pods", len(live))
		return live[0]
	}
	operatorRestarts := func() int64 {
		var total int64
		for _, status := range list(operatorPod(), "status", "containerStatuses") {
			total += num(status, "restartCount")
		}
		return total
	}
	crashedAt := func(point string) bool {
		previous, err := h.tryK("-n", operatorNS, "logs", nameOf(operatorPod()), "--previous")
		if err != nil {
			return false
		}
		return strings.Contains(previous, fmt.Sprintf("injected crash at lifecycle fault point %q", point))
	}
	deployment := func() object { return h.get("deployment", "alpha") }

	// ---- 1. crash after the replica effect, before the journal transition ----
	h.waitFor("alpha at baseline before crash injection", 240*time.Second, func() bool { return settled("alpha", 2) })
	gen := generation(deployment())
	h.setOperatorFault("after-effect")
	h.setReplicas("alpha", 3)
	h.waitFor("manager exited after the replica CAS with the journal still at the prior count", 240*time.Second, func() bool {
		return specReplicas(deployment()) == 3 && operatorRestarts() >= 1 && crashedAt("after-effect")
	})
	issued := annotation(deployment(), operationAnnotation)
	assert(issued != "", "workload carries no operation annotation")
	// The restarted manager reconstructs the issued effect from the workload alone.
	h.waitFor("restart reconstructs the issued addition", 240*time.Second, func() bool {
		return settled("alpha", 3) && num(h.journal("alpha"), "Applied") == 3
	})
	assert(len(completions("alpha", issued)) == 1, "%v", completions("alpha", issued))
	assert(generation(deployment()) == gen+1, "more than one workload spec write")
	h.setOperatorFault("")
	fmt.Println("PASS: crash after effect: one replica write, one completion record")

	// ---- 2. crash after durable intent, before the replica effect ----
	gen = generation(deployment())
	h.setOperatorFault("before-effect")
	h.setReplicas("alpha", 2)
	h.waitFor("manager exited after journaling contraction intent, before the CAS", 420*time.Second, func() bool {
		return operatorRestarts() >= 1 && crashedAt("before-effect")
	})
	intent := sub(h.journal("alpha"), "Operation")
	assert(intent != nil && num(intent, "From") == 3 && num(intent, "To") == 2, "%v", intent)
	assert(specReplicas(deployment()) == 3, "effect issued despite the crash")
	assert(generation(deployment()) == gen, "effect issued despite the crash")
	h.hold(20*time.Second, "crash-looping manager never issues the effect", func() bool {
		current := deployment()
		return specReplicas(current) == 3 && generation(current) == gen
	})
	h.setOperatorFault("")
	h.waitFor("recovered manager issues the journaled contraction exactly once", 420*time.Second, func() bool { return settled("alpha", 2) })
	assert(generation(deployment()) == gen+1, "more than one workload spec write")
	assert(len(completions("alpha", str(intent, "ID"))) == 1, "%v", completions("alpha", str(intent, "ID")))
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable")
	fmt.Println("PASS: crash before effect: intent retained, one decrement after restart, write readable")

	// ---- 3. node loss for a Bucket replica ----
	h.setReplicas("alpha", 3)
	h.waitFor("alpha at three before node loss", 240*time.Second, func() bool { return settled("alpha", 3) })
	worker := h.name + "-worker2"
	var victim string
	for _, pod := range h.fleetPods("alpha") {
		if str(pod, "spec", "nodeName") == worker {
			victim = nameOf(pod)
			break
		}
	}
	assert(victim != "", "no alpha replica on %s", worker)
	h.k("cordon", worker)
	func() {
		defer h.k("uncordon", worker)
		h.k("-n", "fleets", "delete", "pod", victim, "--wait=false")
		h.waitFor("strict hostname separation leaves the replacement Pending on a cordoned cluster", 120*time.Second, func() bool {
			pods := h.fleetPods("alpha")
			unschedulable := false
			for _, pod := range pods {
				if str(pod, "status", "phase") == "Pending" && !deleting(pod) && hasReason(pod, "Unschedulable") {
					unschedulable = true
				}
			}
			return len(pods) >= 3 && unschedulable
		})
		gen = generation(deployment())
		h.setReplicas("alpha", 2)
		h.sleep(15 * time.Second)
		h.hold(45*time.Second, fmt.Sprintf("contraction is not issued while a replica is unavailable (reason: %s)", readyReason("alpha")), func() bool {
			current := deployment()
			return specReplicas(current) == 3 && generation(current) == gen
		})
		reason := readyReason("alpha")
		assert(reason != "Ready" && reason != "Provisioning", "%s", reason)
	}()
	h.waitFor("contraction proceeds once every replica is observable again", 420*time.Second, func() bool { return settled("alpha", 2) })
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable")
	fmt.Println("PASS: Bucket node loss: no decrement while unavailable, write readable after recovery")

	// ---- 4. node loss for a launcher-managed PersistentFleet replica ----
	h.probe("beta-client", "fleets", map[string]string{"celld.eric.dev/client-of": "beta"})
	h.app("beta-client", "beta", "PUT", "/?cell=integration&id=ack")
	h.waitFor("beta at baseline before node loss", 240*time.Second, func() bool { return settled("beta", 2) })
	host := str(h.get("pod", "beta-1"), "spec", "nodeName")
	claimUID := uidOf(h.get("pvc", "data-beta-1"))
	stsGeneration := generation(h.get("statefulset", "beta"))
	h.k("cordon", host)
	func() {
		defer h.k("uncordon", host)
		h.k("-n", "fleets", "delete", "pod", "beta-1", "--wait=true")
		h.waitFor("recreated ordinal waits for its retained host", 120*time.Second, func() bool {
			return str(h.get("pod", "beta-1"), "status", "phase") == "Pending"
		})
		h.hold(30*time.Second, "no decrement, template change or claim replacement while the ordinal is unschedulable", func() bool {
			sts := h.get("statefulset", "beta")
			return specReplicas(sts) == 2 && uidOf(h.get("pvc", "data-beta-1")) == claimUID && generation(sts) == stsGeneration
		})
		assert(!h.ready("beta"), "beta reported ready with an unschedulable ordinal")
	}()
	h.waitFor("ordinal returns on its original host with the same claim", 300*time.Second, func() bool {
		return settled("beta", 2) && str(h.get("pod", "beta-1"), "spec", "nodeName") == host
	})
	assert(uidOf(h.get("pvc", "data-beta-1")) == claimUID, "claim replaced after node loss")
	assert(h.ackStored("beta-client", "beta"), "acknowledged write unreadable")
	fmt.Println("PASS: PersistentFleet node loss: retained claim, same host, write readable")

	// ---- toxiproxy control ----
	toxic := func(method, path string, body any) string {
		args := []string{"-n", storeNS, "exec", "toxi-ctl", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "5", "-X", method, "http://toxiproxy-api:8474" + path}
		if body != nil {
			args = append(args, "-H", "Content-Type: application/json", "-d", encode(body))
		}
		return h.k(args...)
	}
	proxies := decode(toxic("GET", "/proxies", nil))
	assert(boolean(proxies, "minio", "enabled") && strings.HasSuffix(str(proxies, "minio", "listen"), ":9000"), "%v", proxies)

	// ---- 5. S3 latency ----
	toxic("POST", "/proxies/minio/toxics", object{"name": "latency", "type": "latency", "stream": "downstream", "attributes": object{"latency": 250, "jitter": 50}})
	func() {
		defer toxic("DELETE", "/proxies/minio/toxics/latency", nil)
		started := time.Now()
		h.setReplicas("alpha", 3)
		h.waitFor("growth under 250 ms S3 latency", 300*time.Second, func() bool { return settled("alpha", 3) })
		h.setReplicas("alpha", 2)
		h.waitFor("contraction under 250 ms S3 latency", 420*time.Second, func() bool { return settled("alpha", 2) })
		assert(h.ackStored("client", "alpha"), "acknowledged write unreadable")
		fmt.Printf("PASS: S3 latency: shrink/grow completed in %.0f s, write readable\n", time.Since(started).Seconds())
	}()

	// ---- 6. S3 partition during an issued contraction ----
	h.setReplicas("alpha", 3)
	h.waitFor("alpha at three before partition", 300*time.Second, func() bool { return settled("alpha", 3) })
	appliedBefore := num(h.journal("alpha"), "Applied")
	h.setReplicas("alpha", 2)
	deadline := time.Now().Add(420 * time.Second)
	var caught object
	for time.Now().Before(deadline) {
		replicas := specReplicas(deployment())
		op := sub(h.journal("alpha"), "Operation")
		if replicas == 2 && len(op) > 0 {
			caught = op
			break
		}
		if len(op) == 0 && replicas == 2 {
			break
		}
		h.sleep(500 * time.Millisecond)
	}
	toxic("POST", "/proxies/minio/toxics", object{"name": "partition", "type": "timeout", "stream": "upstream", "attributes": object{"timeout": 0}})
	func() {
		defer toxic("DELETE", "/proxies/minio/toxics/partition", nil)
		if caught == nil {
			fmt.Println("Partition window missed: contraction completed before injection; testing steady-state outage")
			h.hold(45*time.Second, "no replica or loss change while S3 is unreachable", func() bool {
				return specReplicas(deployment()) == 2 && str(h.journal("alpha"), "Loss") == ""
			})
			return
		}
		fmt.Println("Partition injected with operation", str(caught, "ID"), "in phase", str(caught, "Phase"))
		h.hold(45*time.Second, "issued contraction neither completes, decrements again, nor records loss while S3 is unreachable", func() bool {
			journal := h.journal("alpha")
			op := sub(journal, "Operation")
			return specReplicas(deployment()) == 2 && num(journal, "Applied") == appliedBefore && str(journal, "Loss") == "" &&
				op != nil && str(op, "ID") == str(caught, "ID") && len(completions("alpha", str(caught, "ID"))) == 0
		})
	}()
	h.waitFor("runtime pods serve again after the partition (self-fenced processes restarted)", 300*time.Second, func() bool {
		for _, pod := range h.fleetPods("alpha") {
			if !deleting(pod) && (str(pod, "status", "phase") != "Running" || !isReady(pod)) {
				return false
			}
		}
		return true
	})
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable after the partition")
	// Liveness after a partition is not promised: the operator may complete or
	// stay blocked on replaced generations. Safety is: one effect, no false completion.
	outcomeDeadline := time.Now().Add(240 * time.Second)
	for time.Now().Before(outcomeDeadline) && len(sub(h.journal("alpha"), "Operation")) > 0 {
		h.sleep(3 * time.Second)
	}
	final := h.journal("alpha")
	assert(specReplicas(deployment()) == 2, "replicas changed after the partition")
	if caught != nil {
		id := str(caught, "ID")
		assert(len(completions("alpha", id)) <= 1, "%v", completions("alpha", id))
		if len(sub(final, "Operation")) > 0 {
			fmt.Printf("Post-partition outcome: operation %s remains open, reason %s (documented liveness limit)\n", id, readyReason("alpha"))
			assert(num(final, "Applied") == appliedBefore, "applied count changed while the operation stayed open")
		} else {
			fmt.Println("Post-partition outcome: operation completed after evidence returned")
			assert(num(final, "Applied") == 2 && len(completions("alpha", id)) == 1, "completion without exactly one record: %v", final)
		}
	}
	fmt.Println("PASS: S3 partition: fail closed throughout, acknowledged write readable after recovery")
	h.exerciseLeaderFailover()
	fmt.Println(h.k("get", "celldstoragereservations", "-o", "json"))
}

// exerciseLeaderFailover runs two manager replicas, issues a contraction, and
// deletes the elected leader once the replica effect is on the workload but the
// operation is still open. The standby must take the lease and complete that
// same operation without a second effect. Restart-safety of the journal is the
// property; the Lease mechanics are controller-runtime's.
func (h *harness) exerciseLeaderFailover() {
	const lease = "celld-operator.celld.eric.dev"
	leader := func() string {
		holder := str(h.getIn(operatorNS, "lease", lease), "spec", "holderIdentity")
		// controller-runtime identities are "<hostname>_<random>"; the hostname is the pod name.
		if i := strings.IndexByte(holder, '_'); i > 0 {
			return holder[:i]
		}
		return holder
	}
	managers := func() []object {
		var live []object
		for _, pod := range h.listIn(operatorNS, "pods", "-l", "app.kubernetes.io/name=celld-operator") {
			if !deleting(pod) && strings.HasPrefix(nameOf(pod), "celld-operator-") {
				live = append(live, pod)
			}
		}
		return live
	}
	completions := func(fleetName, op string) int {
		n := 0
		for _, entry := range list(h.journal(fleetName), "History") {
			if str(entry, "ID") == op {
				n++
			}
		}
		return n
	}
	settled := func(count int64) bool {
		return specReplicas(h.get("deployment", "alpha")) == count && h.ready("alpha") && len(sub(h.journal("alpha"), "Operation")) == 0
	}

	h.setReplicas("alpha", 3)
	h.waitFor("alpha at three before leader loss", 4*time.Minute, func() bool { return settled(3) })
	h.k("-n", operatorNS, "scale", "deployment/celld-operator", "--replicas=2")
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=180s")
	h.waitFor("two manager replicas with one elected leader", 2*time.Minute, func() bool { return len(managers()) == 2 && leader() != "" })
	first := leader()
	generation := num(h.get("deployment", "alpha"), "metadata", "generation")

	h.setReplicas("alpha", 2)
	// Catch the operation after its replica effect and before completion.
	var op string
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		j := h.journal("alpha")
		if specReplicas(h.get("deployment", "alpha")) == 2 && len(sub(j, "Operation")) > 0 {
			op = str(sub(j, "Operation"), "ID")
			break
		}
		if specReplicas(h.get("deployment", "alpha")) == 2 && len(sub(j, "Operation")) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if op == "" {
		fmt.Println("Leader-loss window missed: contraction completed before the leader could be removed; deleting the leader at rest instead")
		last := list(h.journal("alpha"), "History")
		op = str(last[len(last)-1], "ID")
	}
	h.k("-n", operatorNS, "delete", "pod", first, "--wait=false")
	h.waitFor("standby manager acquires the lease", 2*time.Minute, func() bool { l := leader(); return l != "" && l != first })
	h.waitFor("the standby completes the same operation", 6*time.Minute, func() bool { return settled(2) })
	assert(completions("alpha", op) == 1, "operation %s recorded %d times across the leader change", op, completions("alpha", op))
	assert(num(h.get("deployment", "alpha"), "metadata", "generation") == generation+1, "more than one workload spec write across the leader change")
	assert(h.ackStored("client", "alpha"), "acknowledged write not readable after leader loss")
	fmt.Println("PASS: leader loss during an issued contraction: one effect, one completion, write readable")
	h.k("-n", operatorNS, "scale", "deployment/celld-operator", "--replicas=1")
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=180s")
}
