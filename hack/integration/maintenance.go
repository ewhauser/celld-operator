package main

import (
	"fmt"
	"time"
)

func (h *harness) exerciseMaintenance() {
	for _, f := range []struct{ name, probe string }{{"alpha", "client"}, {"beta", "client-beta"}} {
		h.writeLedger(f.probe, f.name)
		before := podUIDs(h.fleetPods(f.name))
		oldClaims := h.claims(f.name)
		token := "strict-restart-" + f.name
		h.merge(f.name, encode(object{"spec": object{"maintenance": object{"restartToken": token, "allowCoordinatedDowntime": true}}}))
		h.waitFor("coordinated restart accepted: "+f.name, 3*time.Minute, func() bool {
			s := h.currentState(f.name)
			return str(s, "RestartToken") == token || str(s, "Operation", "Kind") == "Restart"
		})
		h.restartOperator()
		h.waitFor("restart completes across manager replacement: "+f.name, 10*time.Minute, func() bool { return h.settled(f.name, 2) && str(h.currentState(f.name), "RestartToken") == token })
		after := podUIDs(h.fleetPods(f.name))
		assert(disjoint(before, after), "restart retained old Pod UID")
		h.goneVolumes(oldClaims)
		for name, current := range h.claims(f.name) {
			old := oldClaims[name]
			assert(current.UID != old.UID && current.VolumeUID != old.VolumeUID && current.Handle != old.Handle, "restart reused retired disk")
		}
		h.readLedger(f.probe, f.name)
		h.restartOperator()
		h.hold(12*time.Second, "completed token does not replay: "+f.name, func() bool { return same(podUIDs(h.fleetPods(f.name)), after) })
		if h.opts.upgradeImage != "" {
			upgradePods := podUIDs(h.fleetPods(f.name))
			upgradeClaims := h.claims(f.name)
			h.merge(f.name, encode(object{"spec": object{"runtimeImage": h.opts.upgradeImage}}))
			h.waitFor("fork digest upgrade: "+f.name, 10*time.Minute, func() bool {
				return h.settled(f.name, 2) && str(h.currentState(f.name), "RuntimeImage") == h.opts.upgradeImage
			})
			for _, pod := range h.fleetPods(f.name) {
				assert(str(list(pod, "spec", "containers")[0], "image") == h.opts.upgradeImage, "old runtime digest remains")
			}
			assert(disjoint(upgradePods, podUIDs(h.fleetPods(f.name))), "upgrade retained an old Pod UID")
			h.goneVolumes(upgradeClaims)
			freshClaims := h.claims(f.name)
			assert(len(freshClaims) == len(upgradeClaims), "upgrade changed the expected claim count")
			for name, current := range freshClaims {
				old, captured := upgradeClaims[name]
				assert(captured, "upgrade introduced an unexpected claim: %s", name)
				assert(current.UID != old.UID && current.VolumeUID != old.VolumeUID && current.Handle != old.Handle, "upgrade reused retired PVC/PV/CSI identity: %s", name)
			}
			h.readLedger(f.probe, f.name)
			fmt.Println("PASS:", f.name, "different-digest upgrade replaced every Pod and disk; old PVs and attachments gone; all acknowledged writes preserved")
		}
		oldClaims = h.claims(f.name)
		fleetUID := uidOf(h.get("celldfleet", f.name))
		resUID := uidOf(h.reservation(f.name))
		h.k("-n", "fleets", "delete", "celldfleet", f.name, "--wait=false")
		h.waitFor("strict final deletion: "+f.name, 10*time.Minute, func() bool {
			return h.k("-n", "fleets", "get", "celldfleet", f.name, "--ignore-not-found", "-o", "name") == ""
		})
		assert(len(h.listIn("pods", "-l", "celld.eric.dev/fleet-uid="+fleetUID)) == 0, "runtime Pods survived deletion")
		h.goneVolumes(oldClaims)
		for name := range oldClaims {
			assert(h.k("-n", "fleets", "get", "pvc", name, "--ignore-not-found", "-o", "name") == "", "PVC survived final deletion")
		}
		assert(uidOf(h.reservation(f.name)) == resUID, "bucket reservation lost")
		assert(str(h.currentState(f.name), "Completion", "Kind") == "Delete", "current completion does not record deletion")
		fmt.Println("PASS:", f.name, "compute and disks deleted; bucket reservation retained")
	}
	if h.opts.upgradeImage == "" {
		fmt.Println("NOT RUN: different-digest runtime upgrade; set CELLD_UPGRADE_IMAGE to qualify an actual upgrade")
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
