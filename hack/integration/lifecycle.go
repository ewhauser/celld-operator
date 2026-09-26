package main

import (
	"fmt"
	"strings"
	"time"
)

// exerciseLifecycle qualifies provisioning and scaling for both profiles under
// continuous write load (ADR 0024): a Bucket Deployment and an Ordered Bucket
// scale by setting replicas, and a PersistentFleet removes one member at a
// time, keeps every removed member's disk and reattaches it on growth.
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

	// Growth adds a member on its own claim.
	h.scale("beta", 3)
	grown := h.claims("beta")
	assert(len(grown) == 3, "growth did not create a third claim: %v", grown)

	// Pause stops a contraction before it starts.
	h.merge("beta", `{"spec":{"maintenance":{"paused":true},"replicas":2}}`)
	h.hold(12*time.Second, "pause prevents a new removal", func() bool {
		return specReplicas(h.get("statefulset", "beta")) == 3
	})

	// Contraction on unpause removes one member and keeps its disk.
	h.shrinkWatching(2, func() { h.merge("beta", `{"spec":{"maintenance":null}}`) }, nil)
	must(sameDisks(grown, h.claims("beta")))
	h.readLedger("beta")

	// Growth after a manager restart reattaches the removed member's disk.
	h.restartOperator()
	h.scale("beta", 3)
	must(sameDisks(grown, h.claims("beta")))

	// 3 -> 1 with the manager killed between the two steps; the next step is
	// re-derived from the cluster.
	h.shrinkWatching(1, nil, h.killOperator)
	must(sameDisks(grown, h.claims("beta")))
	h.readLedger("beta")

	// 1 -> 3 in one growth step: both members restart on their old disks.
	h.scale("beta", 3)
	must(sameDisks(grown, h.claims("beta")))
	h.stopWriter("beta")
	h.readLedger("beta")

	// Automatic contraction through Metrics Server uses the same one-member
	// step, with the fleet deliberately idle.
	h.merge("beta", `{"spec":{"capacity":`+fmt.Sprintf(automaticCapacity, 2)+`}}`)
	h.waitSettled("beta", 2, 10*time.Minute)
	h.merge("beta", `{"spec":{"capacity":null,"replicas":2}}`)
	h.waitSettled("beta", 2, 5*time.Minute)
	must(sameDisks(grown, h.claims("beta")))
	h.readLedger("beta")
	fmt.Println("PASS: PersistentFleet removes one member at a time, keeps each removed member's disk, reattaches it on growth, and survives a manager kill mid-contraction")
}

// shrinkWatching contracts beta to target, through trigger or by setting
// replicas, and waits for it to settle. Throughout, no claim of the fleet may
// be deleted and at most one expected member may be down. midway runs once
// after the first member has been removed.
func (h *harness) shrinkWatching(target int, trigger, midway func()) {
	from := specReplicas(h.get("statefulset", "beta"))
	assert(from > int64(target), "beta already has %d members; nothing to contract to %d", from, target)
	d := h.watchDisruptions("beta")
	if trigger == nil {
		trigger = func() { h.setReplicas("beta", target) }
	}
	trigger()
	done := midway == nil
	h.waitWatching(fmt.Sprintf("beta contracts %d -> %d", from, target), 12*time.Minute, d, func() bool {
		for _, claim := range h.listIn("pvc", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", "beta"))) {
			assert(!terminating(claim), "contraction deleted disk %s", nameOf(claim))
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
