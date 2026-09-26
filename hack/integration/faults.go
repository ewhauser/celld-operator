package main

import (
	"fmt"
	"strings"
	"time"
)

// exerciseFaults disrupts one PersistentFleet member at a time in each way a
// kind cluster can (ADR 0023 qualification): graceful and forced Pod
// deletion, SIGKILL of celld, a node drain serialized by the PDB, manager
// kills mid-rollout and mid-contraction, a lost claim, a lost volume and an
// administrator replacement. Continuous writes run through every fault, and
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
			return h.restartCounts("beta")["beta-2"] > h.faultBefore.restarts["beta-2"]
		})
	}, h.sameDisksAfter(), h.restarted("beta-2"))

	h.fault("node drain serialized by the PDB", h.drainTwoNodes, h.sameDisksAfter(), h.newPod("beta-1"))

	h.fault("manager killed mid-contraction", func() {
		h.shrinkWatchingRelease("beta", 2, nil, h.killOperator)
		h.setReplicas("beta", 3)
	})

	h.fault("lost claim", func() {
		h.k("-n", "fleets", "delete", "pvc", "data-beta-1", "--wait=false")
		h.k("-n", "fleets", "delete", "pod", "beta-1", "--wait=false")
		h.waitReplacedDisk("data-beta-1")
	}, h.freshDiskAfter("data-beta-1"))

	h.fault("lost volume", func() {
		// A backend disk that no longer exists: the PV object is removed
		// without its CSI and protection finalizers, as after an EBS loss.
		pv := str(h.get("pvc", "data-beta-0"), "spec", "volumeName")
		h.k("delete", "pv", pv, "--wait=false")
		h.k("patch", "pv", pv, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		h.waitReplacedDisk("data-beta-0")
	}, h.freshDiskAfter("data-beta-0"))

	h.merge("beta", `{"metadata":{"annotations":{"celld.eric.dev/replace-member":"7"}}}`)
	h.wait("replace-member naming no member is reported", func() bool {
		reason, _ := h.fleetReason("beta")
		return reason == "ReplaceMemberInvalid"
	})
	h.k("-n", "fleets", "annotate", "celldfleet", "beta", "celld.eric.dev/replace-member-")
	h.fault("replace-member annotation", func() {
		h.merge("beta", `{"metadata":{"annotations":{"celld.eric.dev/replace-member":"beta-2"}}}`)
		h.waitReplacedDisk("data-beta-2")
	}, h.freshDiskAfter("data-beta-2"), func() {
		assert(annotation(h.get("celldfleet", "beta"), "celld.eric.dev/replace-member") == "", "replace-member annotation was not cleared")
	})

	h.fault("manager killed mid-rollout", func() {
		h.rollAll("beta", encode(object{"spec": object{"maintenance": object{"restartToken": "faults-rollout"}}}), nil, h.killOperator)
	})

	// A Bucket member is disposable: its disk is a cache.
	h.scale("alpha", 2)
	h.writeLedger("alpha")
	h.startWriter("alpha")
	h.k("-n", "fleets", "delete", "pod", "alpha-1", "--force", "--grace-period=0", "--wait=false")
	h.waitSettled("alpha", 2, 5*time.Minute)
	h.stopWriter("alpha")
	h.readLedger("alpha")
	fmt.Println("PASS: every single-member fault was absorbed automatically with every acknowledged write readable")
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
	h.faultBefore = faultSnapshot{claims: before, pods: pods, restarts: h.restartCounts("beta")}
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
		assert(h.restartCounts("beta")[name] > h.faultBefore.restarts[name], "%s container did not restart", name)
	}
}

// freshDiskAfter requires the member's disk replaced and every other disk kept.
func (h *harness) freshDiskAfter(claim string) func() {
	return func() {
		after := h.claims("beta")
		assert(after[claim].UID != h.faultBefore.claims[claim].UID, "%s was not replaced", claim)
		for name, old := range h.faultBefore.claims {
			if name != claim {
				assert(after[name] == old, "replacing %s changed %s", claim, name)
			}
		}
	}
}

// waitReplacedDisk waits for the operator to act on a disk declared or found
// gone: a new claim of the same name. The fleet has not necessarily noticed
// the loss when inject returns, so settling alone would prove nothing.
func (h *harness) waitReplacedDisk(claim string) {
	old := h.faultBefore.claims[claim].UID
	h.waitFor("operator replaces "+claim, 10*time.Minute, func() bool {
		h.observe(h.faultWatch)
		current, ok := h.claims("beta")[claim]
		return ok && current.UID != old
	})
}

func (h *harness) restartCounts(fleetName string) map[string]int64 {
	out := map[string]int64{}
	for _, pod := range h.memberPods(fleetName) {
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
// is on the cordoned node, so the fleet stays unsettled and the budget is 0.
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
	h.wait("PDB allows no disruption while beta recovers", func() bool { return h.budget("beta") == 0 })
	out, err := h.tryK("drain", second, selector, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=30s")
	assert(err != nil, "second drain evicted a member while the fleet was recovering: %s", out)
	assert(uidOf(h.get("pod", "beta-0")) == survivor, "second drain replaced beta-0")
	fmt.Println("PASS: PDB refused a second eviction while a drained member was down")
}
