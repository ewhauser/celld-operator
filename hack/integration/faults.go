package main

import (
	"fmt"
	"strings"
	"time"
)

// exerciseFaults disrupts one PersistentFleet member at a time in each way a
// kind cluster can (ADR 0024 qualification): graceful and forced Pod
// deletion, SIGKILL of celld, a node drain serialized by the PDB, manager
// kills mid-rollout and mid-contraction, a deleted claim, a lost volume and a
// node whose kubelet stops. It then takes every member down at once, by
// SIGKILL and by Pod deletion. Continuous writes run through every fault, and
// after each one the fleet must settle with every acknowledged write readable.
// No step is repaired by hand.
func (h *harness) exerciseFaults() {
	h.scale("beta", 3)
	h.writeLedger("beta")

	h.fault("graceful Pod delete", func() {
		h.k("-n", "fleets", "delete", "pod", "beta-1", "--wait=false")
	}, h.sameDisksAfter(), h.newPod("beta-1"))

	h.fault("forced Pod delete", func() {
		h.k("-n", "fleets", "delete", "pod", "beta-0", "--force", "--grace-period=0", "--wait=false")
	}, h.sameDisksAfter(), h.newPod("beta-0"))

	h.fault("SIGKILL of celld", func() {
		h.sigkill("beta-2")
		h.wait("kubelet restarts the killed celld container", func() bool {
			h.observe(h.faultWatch)
			return h.restartCounts()["beta-2"] > h.faultBefore.restarts["beta-2"]
		})
	}, h.sameDisksAfter(), h.restarted("beta-2"))

	h.fault("node drain serialized by the PDB", h.drainTwoNodes, h.sameDisksAfter(), h.newPod("beta-1"))

	h.fault("manager killed mid-contraction", func() {
		h.shrinkWatching(2, nil, h.killOperator)
		h.setReplicas("beta", 3)
	}, h.sameDisksAfter())

	// The StatefulSet controller recreates a deleted claim for the member's
	// next Pod; the operator takes no part.
	h.fault("deleted claim", func() {
		h.k("-n", "fleets", "delete", "pvc", "data-beta-1", "--wait=false")
		h.k("-n", "fleets", "delete", "pod", "beta-1", "--wait=false")
		h.waitReplacedDisk("data-beta-1", 10*time.Minute)
	}, h.freshDiskAfter("data-beta-1"))

	h.fault("lost volume", func() {
		// A backend disk that no longer exists: the PV object is removed
		// without its CSI and protection finalizers, as after an EBS loss.
		// Kubernetes marks the claim Lost and the operator replaces the member.
		pv := str(h.get("pvc", "data-beta-0"), "spec", "volumeName")
		h.k("delete", "pv", pv, "--wait=false")
		h.k("patch", "pv", pv, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		h.waitReplacedDisk("data-beta-0", 10*time.Minute)
	}, h.freshDiskAfter("data-beta-0"))

	victim, node := h.memberOnSpareNode("beta")
	h.fault("node failure", func() { h.failNode(node, victim) }, h.freshDiskAfter("data-"+victim))

	h.fault("manager killed mid-rollout", func() {
		h.rollAll("beta", encode(object{"spec": object{"maintenance": object{"restartToken": "faults-rollout"}}}), nil, h.killOperator)
	})

	// Every member at once: celld restarts all of them on their own disks and
	// they recover each other's logs, with no action from anyone.
	h.fleetOutage("every celld killed at once", func() {
		for _, pod := range h.memberPods("beta") {
			h.sigkill(nameOf(pod))
		}
		h.wait("kubelet restarts every killed celld container", func() bool {
			after := h.restartCounts()
			for name, before := range h.faultBefore.restarts {
				if after[name] <= before {
					return false
				}
			}
			return true
		})
	}, h.restartedInPlace())
	h.fleetOutage("every Pod deleted at once", func() {
		h.k("-n", "fleets", "delete", "pod", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", "beta")), "--wait=false")
		h.wait("the StatefulSet replaces every Pod", func() bool {
			after := nameUIDs(h.memberPods("beta"))
			for name, uid := range h.faultBefore.pods {
				if after[name] == "" || after[name] == uid {
					return false
				}
			}
			return true
		})
	})

	// A Bucket member is disposable: its disk is a cache.
	h.scale("alpha", 2)
	h.writeLedger("alpha")
	h.startWriter("alpha")
	h.k("-n", "fleets", "delete", "pod", "alpha-1", "--force", "--grace-period=0", "--wait=false")
	h.waitSettled("alpha", 2, 5*time.Minute)
	h.stopWriter("alpha")
	h.readLedger("alpha")
	fmt.Println("PASS: every single-member fault and whole-fleet outage was absorbed automatically with every acknowledged write readable")
}

// fault runs inject against beta under continuous writes, waits for beta to
// settle at three members while watching the one-disruption invariant, runs
// the checks, and verifies the ledger.
func (h *harness) fault(name string, inject func(), checks ...func()) {
	fmt.Println("FAULT:", name)
	h.waitSettled("beta", 3, 10*time.Minute)
	h.startWriter("beta")
	before := h.claims("beta")
	pods := nameUIDs(h.memberPods("beta"))
	h.faultBefore = faultSnapshot{claims: before, pods: pods, restarts: h.restartCounts()}
	d := h.watchDisruptions("beta")
	h.faultWatch = d
	inject()
	h.waitWatching("beta settles after "+name, 15*time.Minute, d, func() bool { return h.settled("beta", 3) })
	for _, check := range checks {
		check()
	}
	h.stopWriter("beta")
	h.readLedger("beta")
	fmt.Println("PASS: fault absorbed:", name)
}

// fleetOutage takes down every beta member at once under continuous writes.
// The one-disruption invariant does not apply; the fleet must settle again on
// its own disks with every acknowledged write readable and no disk replaced.
func (h *harness) fleetOutage(name string, inject func(), checks ...func()) {
	fmt.Println("FAULT:", name)
	h.waitSettled("beta", 3, 10*time.Minute)
	h.startWriter("beta")
	h.faultBefore = faultSnapshot{claims: h.claims("beta"), pods: nameUIDs(h.memberPods("beta")), restarts: h.restartCounts()}
	inject()
	h.waitFor("beta recovers from "+name, 15*time.Minute, func() bool { return h.settled("beta", 3) })
	must(sameDisks(h.faultBefore.claims, h.claims("beta")))
	for _, check := range checks {
		check()
	}
	h.stopWriter("beta")
	h.readLedger("beta")
	fmt.Println("PASS: whole-fleet outage absorbed:", name)
}

// restartedInPlace requires every member to keep its Pod: kubelet restarted
// the containers and nothing replaced a member.
func (h *harness) restartedInPlace() func() {
	return func() {
		assert(same(nameUIDs(h.memberPods("beta")), h.faultBefore.pods), "a member Pod was replaced after its container was killed")
	}
}

type faultSnapshot struct {
	claims   map[string]claimIdentity
	pods     map[string]string
	restarts map[string]int64
}

func (h *harness) sameDisksAfter() func() {
	return func() { must(sameDisks(h.faultBefore.claims, h.claims("beta"))) }
}

func (h *harness) newPod(name string) func() {
	return func() {
		assert(nameUIDs(h.memberPods("beta"))[name] != h.faultBefore.pods[name], "%s kept its Pod", name)
	}
}

func (h *harness) restarted(name string) func() {
	return func() {
		assert(nameUIDs(h.memberPods("beta"))[name] == h.faultBefore.pods[name], "%s was replaced instead of restarted", name)
		assert(h.restartCounts()[name] > h.faultBefore.restarts[name], "%s container did not restart", name)
	}
}

// freshDiskAfter requires the member's disk replaced and every other disk kept.
func (h *harness) freshDiskAfter(claim string) func() {
	return func() {
		after := h.claims("beta")
		assert(freshDisk(h.faultBefore.claims[claim], after[claim]), "%s was not replaced by a new disk: %+v", claim, after[claim])
		for name, old := range h.faultBefore.claims {
			if name != claim {
				assert(after[name] == old, "replacing %s changed %s", claim, name)
			}
		}
	}
}

// waitReplacedDisk waits for a new claim of the same name after a claim was
// deleted. The fleet has not necessarily noticed the loss when inject
// returns, so settling alone would prove nothing.
func (h *harness) waitReplacedDisk(claim string, timeout time.Duration) {
	old := h.faultBefore.claims[claim].UID
	h.waitFor("replacement of "+claim, timeout, func() bool {
		h.observe(h.faultWatch)
		current, ok := h.claims("beta")[claim]
		return ok && current.UID != old
	})
}

// restartCounts reports the celld container restart count of each beta member.
func (h *harness) restartCounts() map[string]int64 {
	out := map[string]int64{}
	for _, pod := range h.memberPods("beta") {
		for _, c := range list(pod, "status", "containerStatuses") {
			if str(c, "name") == "celld" {
				out[nameOf(pod)] = num(c, "restartCount")
			}
		}
	}
	return out
}

// sigkill kills celld's process from its node, bypassing SIGTERM's drain.
func (h *harness) sigkill(podName string) {
	pod := h.get("pod", podName)
	node := str(pod, "spec", "nodeName")
	var id string
	for _, c := range list(pod, "status", "containerStatuses") {
		if str(c, "name") == "celld" {
			_, id, _ = strings.Cut(str(c, "containerID"), "://")
		}
	}
	assert(node != "" && id != "", "cannot locate celld container of %s", podName)
	pid := strings.TrimSpace(h.run(command{args: []string{"docker", "exec", node, "crictl", "inspect", "--output", "go-template", "--template", "{{.info.pid}}", id}, timeout: 30 * time.Second}))
	h.sh(30*time.Second, "docker", "exec", node, "kill", "-9", pid)
}

// drainTwoNodes drains beta-1's node, which the budget allows, and then
// requires a second drain to be refused while beta-1 cannot return: its disk
// is on the cordoned node, so beta-1 stays unready and the fixed budget of one
// admits no further eviction.
func (h *harness) drainTwoNodes() {
	selector := "--pod-selector=celld.eric.dev/fleet-uid=" + uidOf(h.get("celldfleet", "beta"))
	first := str(h.get("pod", "beta-1"), "spec", "nodeName")
	second := str(h.get("pod", "beta-0"), "spec", "nodeName")
	survivor := uidOf(h.get("pod", "beta-0"))
	defer func() {
		h.k("uncordon", first)
		h.k("uncordon", second)
	}()
	h.k("drain", first, selector, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=180s")
	h.wait("PDB allows no disruption while beta-1 is down", func() bool { return h.budget("beta") == 1 && h.disruptionsAllowed("beta") == 0 })
	out, err := h.tryK("drain", second, selector, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=30s")
	assert(err != nil, "second drain evicted a member while the fleet was recovering: %s", out)
	assert(uidOf(h.get("pod", "beta-0")) == survivor, "second drain replaced beta-0")
	fmt.Println("PASS: PDB refused a second eviction while a drained member was down")
}

// memberOnSpareNode picks a member on a node that neither the store nor the
// operator runs on, so failing that node disrupts only the fleet.
func (h *harness) memberOnSpareNode(fleetName string) (member, node string) {
	store := str(h.getIn(storeNS, "pod", "minio"), "spec", "nodeName")
	for _, pod := range h.memberPods(fleetName) {
		if n := str(pod, "spec", "nodeName"); n != "" && n != h.nodes[0] && n != store {
			return nameOf(pod), n
		}
	}
	fail("no %s member runs on a node free of the store and the operator", fleetName)
	return "", ""
}

// failNode stops a node's kubelet, as when a node hangs or loses the control
// plane. Its containers keep running, so celld on it becomes a zombie. After
// the unreachable-node toleration the member's Pod is evicted and stays
// Terminating, since nothing confirms it stopped. The operator must
// force-delete it and, once the member has been down for the replacement
// delay, replace its disk. The kubelet returns only after both, and the
// member rejoins on a fresh disk.
func (h *harness) failNode(node, member string) {
	old := h.faultBefore.pods[member]
	h.sh(time.Minute, "docker", "exec", node, "systemctl", "stop", "kubelet")
	defer h.sh(time.Minute, "docker", "exec", node, "systemctl", "start", "kubelet")
	h.waitFor("operator force-deletes "+member+" from the failed node", 15*time.Minute, func() bool {
		h.observe(h.faultWatch)
		pod, err := h.tryGet("fleets", "pod", member)
		return err != nil || uidOf(pod) != old
	})
	h.waitReplacedDisk("data-"+member, memberReplacementDelay+10*time.Minute)
}
