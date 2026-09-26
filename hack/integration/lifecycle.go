package main

import (
	"fmt"
	"strings"
	"time"
)

// exerciseLifecycle qualifies provisioning and scaling for both profiles under
// continuous write load (ADR 0023): a Bucket Deployment and an Ordered Bucket
// scale by setting replicas, and a PersistentFleet removes one member at a
// time, deleting its disk only after release and growing onto fresh claims.
func (h *harness) exerciseLifecycle() {
	h.exerciseBucketDeployment()
	h.exerciseOrderedBucket()
	h.exercisePersistentScaling()
}

func (h *harness) exerciseBucketDeployment() {
	gamma := h.bucketFleet("gamma", "bucket-gamma")
	gamma.Spec.BucketWorkload = "Deployment"
	h.apply(gamma)
	h.waitSettled("gamma", 2, 5*time.Minute)
	assert(h.workloadKind("gamma") == "deployment", "Bucket Deployment fleet is not a Deployment")
	assert(strings.TrimSpace(h.k("-n", "fleets", "get", "statefulset", "gamma", "--ignore-not-found", "-o", "name")) == "", "Bucket Deployment fleet created a StatefulSet")
	h.assertDirectRuntime("gamma")
	h.writeLedger("gamma")
	h.startWriter("gamma")
	h.scale("gamma", 3)
	h.scale("gamma", 2)
	h.stopWriter("gamma")
	h.readLedger("gamma")
	h.deleteFleet("gamma")
	fmt.Println("PASS: Bucket Deployment provisions, scales both ways under write load and deletes")
}

func (h *harness) exerciseOrderedBucket() {
	h.scale("alpha", 2)
	h.writeLedger("alpha")
	h.startWriter("alpha")
	h.scale("alpha", 3)
	before := nameUIDs(h.memberPods("alpha"))
	h.scale("alpha", 2)
	after := nameUIDs(h.memberPods("alpha"))
	assert(len(after) == 2 && after["alpha-0"] == before["alpha-0"] && after["alpha-1"] == before["alpha-1"], "Ordered Bucket contraction did not remove exactly the highest ordinal: %v -> %v", before, after)
	h.scale("alpha", 1)
	h.readLedger("alpha")
	h.scale("alpha", 2)
	h.stopWriter("alpha")
	h.readLedger("alpha")
	h.assertDirectRuntime("alpha")
	fmt.Println("PASS: Ordered Bucket grows, removes the highest ordinal, reaches 1 and regrows under write load")
}

func (h *harness) exercisePersistentScaling() {
	h.scale("beta", 2)
	h.writeLedger("beta")
	h.startWriter("beta")

	// Growth onto a fresh claim.
	h.scale("beta", 3)
	grown := h.claims("beta")
	assert(len(grown) == 3, "growth did not create a third claim: %v", grown)

	// Pause stops a contraction before it starts.
	h.merge("beta", `{"spec":{"maintenance":{"paused":true},"replicas":2}}`)
	h.hold(12*time.Second, "pause prevents a new removal", func() bool {
		_, kept := h.claims("beta")["data-beta-2"]
		return specReplicas(h.get("statefulset", "beta")) == 3 && kept
	})

	// Contraction on unpause: one member, and its disk only after the
	// member is gone.
	h.shrinkWatchingRelease(2, func() { h.merge("beta", `{"spec":{"maintenance":null}}`) }, nil)
	survivors := h.claims("beta")
	for name, id := range survivors {
		assert(grown[name] == id, "contraction changed surviving disk %s", name)
	}
	h.goneVolumes(map[string]claimIdentity{"data-beta-2": grown["data-beta-2"]})
	h.readLedger("beta")

	// Growth after a manager restart never reuses the removed member's disk.
	h.restartOperator()
	h.scale("beta", 3)
	regrown := h.claims("beta")
	assert(freshDisk(grown["data-beta-2"], regrown["data-beta-2"]), "growth reused the removed member's disk identity")

	// 3 -> 1 with the manager killed between the two steps; the next step is
	// re-derived from the cluster.
	h.shrinkWatchingRelease(1, nil, h.killOperator)
	assert(h.claims("beta")["data-beta-0"] == regrown["data-beta-0"], "contraction to one member changed the survivor's disk")
	h.readLedger("beta")

	// 1 -> 3 in one growth step, onto fresh claims.
	h.scale("beta", 3)
	final := h.claims("beta")
	for _, name := range []string{"data-beta-1", "data-beta-2"} {
		assert(freshDisk(regrown[name], final[name]), "regrowth reused disk %s", name)
	}
	h.stopWriter("beta")
	h.readLedger("beta")

	// Automatic contraction through Metrics Server uses the same settled,
	// one-member removal. The fleet is deliberately idle here: an idle
	// leader must still move its ensemble off a departed follower, or the
	// removed member's disk is never released (fixed in celld after
	// v0.5.1-ewhauser.5).
	h.merge("beta", `{"spec":{"capacity":`+fmt.Sprintf(automaticCapacity, 2)+`}}`)
	h.waitSettled("beta", 2, 10*time.Minute)
	h.merge("beta", `{"spec":{"capacity":null,"replicas":2}}`)
	h.waitSettled("beta", 2, 5*time.Minute)
	h.readLedger("beta")
	fmt.Println("PASS: PersistentFleet grows onto fresh disks, removes one member at a time, releases disks after removal, survives a manager kill mid-contraction")
}

// shrinkWatchingRelease contracts beta to target, through trigger or by
// setting replicas, and waits for it to settle. Throughout, a removed member's
// disk must outlive its Pod, and at most one expected member may be down.
// midway runs once after the first member has been removed.
func (h *harness) shrinkWatchingRelease(target int, trigger, midway func()) {
	from := specReplicas(h.get("statefulset", "beta"))
	assert(from > int64(target), "beta already has %d members; nothing to contract to %d", from, target)
	d := h.watchDisruptions("beta")
	if trigger == nil {
		trigger = func() { h.setReplicas("beta", target) }
	}
	trigger()
	done := midway == nil
	h.waitWatching(fmt.Sprintf("beta contracts %d -> %d", from, target), 12*time.Minute, d, func() bool {
		pods := map[string]bool{}
		for _, pod := range h.memberPods("beta") {
			pods[nameOf(pod)] = true
		}
		for _, claim := range h.listIn("pvc", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", "beta"))) {
			member := strings.TrimPrefix(nameOf(claim), "data-")
			assert(!terminating(claim) || !pods[member], "disk %s was deleted while member %s still existed", nameOf(claim), member)
		}
		if !done && specReplicas(h.get("statefulset", "beta")) < from {
			midway()
			done = true
		}
		return h.settled("beta", int64(target))
	})
}

// deleteFleet deletes a fleet and requires its compute and disks gone and its
// bucket reservation retained.
func (h *harness) deleteFleet(fleetName string) {
	fleetUID := uidOf(h.get("celldfleet", fleetName))
	resUID := uidOf(h.reservation(fleetName))
	claims := h.claims(fleetName)
	h.k("-n", "fleets", "delete", "celldfleet", fleetName, "--wait=false")
	h.waitFor("fleet deletion: "+fleetName, 10*time.Minute, func() bool {
		return h.k("-n", "fleets", "get", "celldfleet", fleetName, "--ignore-not-found", "-o", "name") == ""
	})
	assert(len(h.listIn("pods", "-l", "celld.eric.dev/fleet-uid="+fleetUID)) == 0, "runtime Pods survived deletion")
	assert(len(h.listIn("pvc", "-l", "celld.eric.dev/fleet-uid="+fleetUID)) == 0, "PVCs survived deletion")
	for _, kind := range []string{"statefulset", "deployment"} {
		assert(h.k("-n", "fleets", "get", kind, fleetName, "--ignore-not-found", "-o", "name") == "", "%s survived deletion", kind)
	}
	h.waitFor("deleted fleet's volumes released: "+fleetName, 3*time.Minute, func() bool {
		out := h.k("get", "pv", "-o", "json")
		for _, old := range claims {
			if strings.Contains(out, `"name":"`+old.Volume+`"`) {
				return false
			}
		}
		return true
	})
	h.goneVolumes(claims)
	assert(uidOf(h.reservation(fleetName)) == resUID, "bucket reservation lost")
	fmt.Println("PASS:", fleetName, "compute and disks deleted; bucket reservation retained")
}
