package main

import (
	"fmt"
	"strings"
	"time"
)

// exerciseMaintenance qualifies rolling restart and runtime upgrade on
// retained disks (ADR 0024): a restart goes one member at a time, highest
// ordinal first, with no coordinated downtime; an upgrade across an
// incompatible runtime pair is the documented full stop. Every PVC/PV/CSI
// identity stays unchanged and every acknowledged write readable. It ends with deletion of both profiles.
func (h *harness) exerciseMaintenance() {
	h.exercisePersistentRestart()
	h.exercisePersistentUpgrade()
	h.exerciseBucketRestart()
	h.deleteFleet("beta")
	h.deleteFleet("alpha")
}

// rollAll applies patch to a PersistentFleet and waits until every member
// has a new Pod on its old disk, watching the one-disruption invariant.
// midway, when set, runs once after the first member has been replaced.
func (h *harness) rollAll(fleetName, patch string, done func() bool, midway func()) {
	count := specReplicas(h.get("statefulset", fleetName))
	disks := h.claims(fleetName)
	before := nameUIDs(h.memberPods(fleetName))
	d := h.watchDisruptions(fleetName)
	h.merge(fleetName, patch)
	h.waitWatching("rolling "+fleetName+" on retained disks", 20*time.Minute, d, func() bool {
		if midway != nil && len(d.replaced) > 0 {
			midway()
			midway = nil
		}
		after := nameUIDs(h.memberPods(fleetName))
		for name, uid := range before {
			if after[name] == uid {
				return false
			}
		}
		return h.settled(fleetName, count) && (done == nil || done())
	})
	must(sameDisks(disks, h.claims(fleetName)))
	want := make([]string, 0, count)
	for i := count - 1; i >= 0; i-- {
		want = append(want, fmt.Sprintf("%s-%d", fleetName, i))
	}
	assert(same(d.replaced, want), "%s members were not replaced highest ordinal first: %v", fleetName, d.replaced)
	fmt.Println("PASS:", fleetName, "rolled one member at a time, highest ordinal first; every disk retained")
}

func (h *harness) exercisePersistentRestart() {
	h.scale("beta", 3)
	h.writeLedger("beta")
	h.startWriter("beta")
	before := nameUIDs(h.memberPods("beta"))
	token := "rolling-restart-beta"
	// A paused fleet accepts the token but changes nothing.
	h.merge("beta", encode(object{"spec": object{"maintenance": object{"paused": true, "restartToken": token}}}))
	h.hold(15*time.Second, "pause holds a requested restart", func() bool { return same(nameUIDs(h.memberPods("beta")), before) })
	h.rollAll("beta", `{"spec":{"maintenance":{"paused":false}}}`, nil, nil)
	h.stopWriter("beta")
	h.readLedger("beta")
	after := nameUIDs(h.memberPods("beta"))
	h.restartOperator()
	h.hold(20*time.Second, "completed restart token does not replay", func() bool { return same(nameUIDs(h.memberPods("beta")), after) })
}

func (h *harness) exercisePersistentUpgrade() {
	if h.opts.upgradeFrom == "" {
		fmt.Println("NOT RUN: runtime upgrade on retained disks; --upgrade-from none")
		return
	}
	legacy := h.newFleet("legacy", "bucket-legacy", "PersistentFleet", "fleets")
	legacy.Spec.RuntimeImage = h.opts.upgradeFrom
	legacy.Spec.Replicas = 3
	h.apply(legacy)
	h.waitSettled("legacy", 3, 8*time.Minute)
	fmt.Println("PASS: a runtime without node_log state provisions like any other")
	h.writeLedger("legacy")
	h.startWriter("legacy")
	upgraded := func() bool {
		for _, pod := range h.memberPods("legacy") {
			if str(list(pod, "spec", "containers")[0], "image") != h.opts.runtimeImage {
				return false
			}
		}
		return true
	}
	if h.opts.upgradeMode == "full-stop" {
		h.fullStopUpgrade("legacy", h.opts.runtimeImage)
		assert(upgraded(), "a member of legacy returned on another image")
	} else {
		h.rollAll("legacy", encode(object{"spec": object{"runtimeImage": h.opts.runtimeImage}}), upgraded, nil)
	}
	h.stopWriter("legacy")
	h.readLedger("legacy")
	fmt.Println("PASS: runtime upgrade", h.opts.upgradeFrom, "->", h.opts.runtimeImage, "("+h.opts.upgradeMode+") kept every disk and acknowledged write")
	h.deleteFleet("legacy")
}

func (h *harness) exerciseBucketRestart() {
	h.scale("alpha", 2)
	h.writeLedger("alpha")
	h.startWriter("alpha")
	before := nameUIDs(h.memberPods("alpha"))
	h.merge("alpha", encode(object{"spec": object{"maintenance": object{"restartToken": "rolling-restart-alpha"}}}))
	h.waitFor("Ordered Bucket rolling restart", 10*time.Minute, func() bool {
		after := nameUIDs(h.memberPods("alpha"))
		for name, uid := range before {
			if after[name] == uid {
				return false
			}
		}
		return h.settled("alpha", 2)
	})
	assert(h.budget("alpha") == 1, "Bucket PDB does not allow one disruption")
	h.stopWriter("alpha")
	h.readLedger("alpha")
	fmt.Println("PASS: Ordered Bucket rolling restart replaced every member with acknowledged writes intact")
}

// fullStopUpgrade runs the documented full-stop runtime upgrade, command for
// command (site/src/content/docs/operate/upgrade-runtime.md): pause, stop
// every member, then set the new image and resume in one change. Every member
// must return on its own disk.
func (h *harness) fullStopUpgrade(fleetName, image string) {
	count := specReplicas(h.get("statefulset", fleetName))
	disks := h.claims(fleetName)
	uid := str(h.get("celldfleet", fleetName), "metadata", "uid")
	h.merge(fleetName, `{"spec":{"maintenance":{"paused":true}}}`)
	h.k("-n", "fleets", "wait", "celldfleet/"+fleetName, "--for=condition=MaintenancePaused", "--timeout=2m")
	h.k("-n", "fleets", "scale", "statefulset", fleetName, "--replicas=0")
	h.waitFor("every "+fleetName+" member stopped", 15*time.Minute, func() bool {
		return strings.TrimSpace(h.k("-n", "fleets", "get", "pods", "-l", "celld.eric.dev/fleet-uid="+uid, "-o", "name")) == ""
	})
	h.merge(fleetName, encode(object{"spec": object{"runtimeImage": image, "maintenance": object{"paused": false}}}))
	h.k("-n", "fleets", "wait", "celldfleet/"+fleetName, "--for=condition=Ready", "--timeout=20m")
	h.waitSettled(fleetName, count, 5*time.Minute)
	must(sameDisks(disks, h.claims(fleetName)))
	fmt.Println("PASS:", fleetName, "full-stop upgrade brought every member back on its own disk")
}
