package main

import (
	"fmt"
	"slices"
	"time"
)

type claimIdentity struct{ UID, Volume, VolumeUID, Handle string }

func (h *harness) settled(fleetName string, count int64) bool {
	return h.ready(fleetName) && specReplicas(h.get(h.workloadKind(fleetName), fleetName)) == count && len(sub(h.currentState(fleetName), "Operation")) == 0 && num(h.currentState(fleetName), "Applied") == count
}

func (h *harness) scale(fleetName string, count int) {
	h.setReplicas(fleetName, count)
	h.waitFor(fmt.Sprintf("%s strict scale to %d", fleetName, count), 8*time.Minute, func() bool { return h.settled(fleetName, int64(count)) })
}

func (h *harness) claims(fleetName string) map[string]claimIdentity {
	out := map[string]claimIdentity{}
	for _, claim := range h.listIn("pvc", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", fleetName))) {
		pv := h.cluster("pv", str(claim, "spec", "volumeName"))
		assert(str(pv, "spec", "persistentVolumeReclaimPolicy") == "Delete", "PV does not use Delete")
		assert(str(pv, "spec", "csi", "driver") == "hostpath.csi.k8s.io", "PV has no hostpath CSI identity")
		assert(slices.Contains(strs(pv, "metadata", "finalizers"), "external-provisioner.volume.kubernetes.io/finalizer"), "PV lacks backend deletion finalizer")
		assert(same(strs(claim, "spec", "accessModes"), []string{"ReadWriteOncePod"}), "claim is not RWOP")
		out[nameOf(claim)] = claimIdentity{uidOf(claim), nameOf(pv), uidOf(pv), str(pv, "spec", "csi", "volumeHandle")}
	}
	return out
}

func (h *harness) goneVolumes(claims map[string]claimIdentity) {
	for name, old := range claims {
		assert(h.k("get", "pv", old.Volume, "--ignore-not-found", "-o", "name") == "", "old PV still exists for %s", name)
		for _, va := range items(decode(h.k("get", "volumeattachments", "-o", "json"))) {
			assert(str(va, "spec", "source", "persistentVolumeName") != old.Volume, "old attachment remains for %s", name)
		}
	}
}

func (h *harness) writeLedger(probe, fleetName string) {
	if h.ledgers == nil {
		h.ledgers = map[string][]string{}
	}
	batch := fmt.Sprintf("ack-%d", time.Now().UnixNano())
	for i := range 12 {
		path := fmt.Sprintf("/?cell=integration&id=%s-%d", batch, i)
		assert(stored(h.app(probe, fleetName, "PUT", path)), "write %s not acknowledged", path)
		h.ledgers[fleetName] = append(h.ledgers[fleetName], path)
	}
}
func (h *harness) readLedger(probe, fleetName string) {
	paths := h.ledgers[fleetName]
	assert(len(paths) > 0, "no acknowledged ledger writes for %s", fleetName)
	for _, path := range paths {
		assert(stored(h.app(probe, fleetName, "GET", path)), "acknowledged write lost: %s %s", fleetName, path)
	}
	fmt.Printf("PASS: %s %d/%d acknowledged writes readable\n", fleetName, len(paths), len(paths))
}

func (h *harness) exerciseLifecycle() {
	for _, f := range []struct{ name, probe string }{{"alpha", "client"}, {"beta", "client-beta"}} {
		h.writeLedger(f.probe, f.name)
		h.scale(f.name, 3)
		before := nameUIDs(h.fleetPods(f.name))
		oldClaims := h.claims(f.name)
		h.merge(f.name, `{"spec":{"maintenance":{"paused":true},"replicas":2}}`)
		h.hold(12*time.Second, "pause prevents new removal: "+f.name, func() bool {
			return specReplicas(h.get("statefulset", f.name)) == 3 && len(sub(h.currentState(f.name), "Operation")) == 0
		})
		h.merge(f.name, `{"spec":{"maintenance":null}}`)
		h.scale(f.name, 2)
		for _, pod := range h.fleetPods(f.name) {
			assert(before[nameOf(pod)] == uidOf(pod), "surviving ordinal replaced")
		}
		if old, ok := oldClaims["data-"+f.name+"-2"]; ok {
			h.goneVolumes(map[string]claimIdentity{"retired": old})
			assert(h.k("-n", "fleets", "get", "pvc", "data-"+f.name+"-2", "--ignore-not-found", "-o", "name") == "", "retired claim remains")
		}
		h.readLedger(f.probe, f.name)
		h.restartOperator()
		h.scale(f.name, 3)
		assert(uidOf(h.get("pod", f.name+"-2")) != before[f.name+"-2"], "growth reused old Pod UID")
		for name, current := range h.claims(f.name) {
			if name == "data-"+f.name+"-2" {
				old := oldClaims[name]
				assert(current.UID != old.UID && current.VolumeUID != old.VolumeUID && current.Handle != old.Handle, "growth reused retired disk identity")
			}
		}
		h.scale(f.name, 2)
		h.scale(f.name, 1)
		h.readLedger(f.probe, f.name)
		h.scale(f.name, 2)
		h.readLedger(f.probe, f.name)
		h.scale(f.name, 3)
		h.merge(f.name, `{"spec":{"capacity":`+fmt.Sprintf(automaticCapacity, 2)+`}}`)
		h.waitFor("Metrics Server automatic contraction: "+f.name, 8*time.Minute, func() bool { return h.settled(f.name, 2) })
		h.merge(f.name, `{"spec":{"capacity":null,"replicas":2}}`)
		h.readLedger(f.probe, f.name)
		s := h.currentState(f.name)
		assert(s["History"] == nil && s["BucketHistory"] == nil && s["Sessions"] == nil, "historical lifecycle state retained")
		assert(len(encode(s)) < 4096, "idle state did not remain bounded: %d bytes", len(encode(s)))
		fmt.Println("PASS:", f.name, "fresh-disk grow, exact-ordinal shrink, 2-to-1, controller restart, bounded idle state")
	}
}

func nameUIDs(pods []object) map[string]string {
	out := map[string]string{}
	for _, pod := range pods {
		out[nameOf(pod)] = uidOf(pod)
	}
	return out
}
