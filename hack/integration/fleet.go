package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type claimIdentity struct{ UID, Volume, VolumeUID, Handle string }

// memberPods lists the fleet's workload Pods, including terminating ones.
// Probe Pods that carry the fleet label for network identity are excluded.
func (h *harness) memberPods(fleetName string) []object {
	var out []object
	for _, pod := range h.fleetPods(fleetName) {
		for _, owner := range list(pod, "metadata", "ownerReferences") {
			if kind := str(owner, "kind"); kind == "StatefulSet" || kind == "ReplicaSet" {
				out = append(out, pod)
				break
			}
		}
	}
	return out
}

func terminating(pod object) bool { return str(pod, "metadata", "deletionTimestamp") != "" }

func podReady(pod object) bool {
	if terminating(pod) {
		return false
	}
	for _, c := range conditions(pod) {
		if str(c, "type") == "Ready" {
			return str(c, "status") == "True"
		}
	}
	return false
}

// settled is the harness's own view of a finished change: the operator
// reports Provisioned for the current generation, the workload has observed
// its spec with exactly count updated and ready replicas, no fleet Pod is
// still terminating (a removed member is gone, not draining), and a
// PersistentFleet retains no disk above count and allows one disruption.
func (h *harness) settled(fleetName string, count int64) bool {
	f := h.get("celldfleet", fleetName)
	ready := condition(f, "Ready")
	if ready == nil || str(ready, "status") != "True" || str(ready, "reason") != "Provisioned" || num(ready, "observedGeneration") != generation(f) || num(f, "status", "observedGeneration") != generation(f) {
		return false
	}
	kind := h.workloadKind(fleetName)
	w, err := h.tryGet("fleets", kind, fleetName)
	if err != nil || specReplicas(w) != count || num(w, "status", "observedGeneration") < generation(w) {
		return false
	}
	if num(w, "status", "replicas") != count || num(w, "status", "readyReplicas") != count || num(w, "status", "updatedReplicas") != count {
		return false
	}
	pods := h.memberPods(fleetName)
	if int64(len(pods)) != count {
		return false
	}
	revision := str(w, "status", "updateRevision")
	for _, pod := range pods {
		if !podReady(pod) {
			return false
		}
		if kind == "statefulset" && str(pod, "metadata", "labels", "controller-revision-hash") != revision {
			return false
		}
	}
	if str(f, "spec", "profile") != "PersistentFleet" {
		return true
	}
	for name := range h.claims(fleetName) {
		if ordinal, ok := claimOrdinal(fleetName, name); !ok || int64(ordinal) >= count {
			return false
		}
	}
	return h.budget(fleetName) == 1
}

// budget reports the fleet PDB's maxUnavailable, or -1 when absent.
func (h *harness) budget(fleetName string) int64 {
	pdb, err := h.tryGet("fleets", "pdb", fleetName)
	if err != nil {
		return -1
	}
	if v, ok := field(pdb, "spec", "maxUnavailable").(float64); ok {
		return int64(v)
	}
	return -1
}

func claimOrdinal(fleetName, claim string) (int, bool) {
	raw, ok := strings.CutPrefix(claim, "data-"+fleetName+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && strconv.Itoa(n) == raw
}

func (h *harness) waitSettled(fleetName string, count int64, timeout time.Duration) {
	h.waitFor(fmt.Sprintf("%s settled at %d members", fleetName, count), timeout, func() bool { return h.settled(fleetName, count) })
}

func (h *harness) scale(fleetName string, count int) {
	h.setReplicas(fleetName, count)
	h.waitSettled(fleetName, int64(count), 10*time.Minute)
}

func (h *harness) claims(fleetName string) map[string]claimIdentity {
	out := map[string]claimIdentity{}
	for _, claim := range h.listIn("pvc", "-l", "celld.eric.dev/fleet-uid="+uidOf(h.get("celldfleet", fleetName))) {
		if terminating(claim) || str(claim, "spec", "volumeName") == "" {
			out[nameOf(claim)] = claimIdentity{UID: uidOf(claim)}
			continue
		}
		pv, err := h.tryGet("", "pv", str(claim, "spec", "volumeName"))
		if err != nil {
			out[nameOf(claim)] = claimIdentity{UID: uidOf(claim)}
			continue
		}
		assert(str(pv, "spec", "persistentVolumeReclaimPolicy") == "Delete", "PV does not use Delete")
		assert(str(pv, "spec", "csi", "driver") == "hostpath.csi.k8s.io", "PV has no hostpath CSI identity")
		assert(slices.Contains(strs(pv, "metadata", "finalizers"), "external-provisioner.volume.kubernetes.io/finalizer"), "PV lacks backend deletion finalizer")
		assert(same(strs(claim, "spec", "accessModes"), []string{"ReadWriteOncePod"}), "claim is not RWOP")
		out[nameOf(claim)] = claimIdentity{uidOf(claim), nameOf(pv), uidOf(pv), str(pv, "spec", "csi", "volumeHandle")}
	}
	return out
}

// sameDisks requires every claim to keep its PVC, PV and CSI identity.
func sameDisks(before, after map[string]claimIdentity) error {
	if len(before) != len(after) {
		return fmt.Errorf("claim set changed: %v -> %v", before, after)
	}
	for name, old := range before {
		if after[name] != old {
			return fmt.Errorf("disk %s changed identity: %+v -> %+v", name, old, after[name])
		}
	}
	return nil
}

func freshDisk(old, current claimIdentity) bool {
	return current.UID != old.UID && current.VolumeUID != old.VolumeUID && current.Handle != old.Handle
}

func (h *harness) goneVolumes(claims map[string]claimIdentity) {
	for name, old := range claims {
		assert(h.k("get", "pv", old.Volume, "--ignore-not-found", "-o", "name") == "", "old PV still exists for %s", name)
		for _, va := range items(decode(h.k("get", "volumeattachments", "-o", "json"))) {
			assert(str(va, "spec", "source", "persistentVolumeName") != old.Volume, "old attachment remains for %s", name)
		}
	}
}

func nameUIDs(pods []object) map[string]string {
	out := map[string]string{}
	for _, pod := range pods {
		if !terminating(pod) {
			out[nameOf(pod)] = uidOf(pod)
		}
	}
	return out
}

// disruptions watches one PersistentFleet while a change runs. Each sample
// counts expected members (ordinals below the workload's replicas) that are
// missing, terminating or unready, and requires at most one. It also records
// the order in which members received a new Pod.
type disruptions struct {
	fleet    string
	uids     map[string]string
	replaced []string
	maxDown  int
}

func (h *harness) watchDisruptions(fleetName string) *disruptions {
	return &disruptions{fleet: fleetName, uids: nameUIDs(h.memberPods(fleetName))}
}

func (h *harness) observe(d *disruptions) {
	w, err := h.tryGet("fleets", "statefulset", d.fleet)
	if err != nil {
		return
	}
	pods := map[string]object{}
	for _, pod := range h.memberPods(d.fleet) {
		pods[nameOf(pod)] = pod
	}
	down := downMembers(d.fleet, specReplicas(w), pods)
	d.maxDown = max(d.maxDown, len(down))
	assert(len(down) <= 1, "more than one %s member disrupted at once: %v", d.fleet, down)
	for name, pod := range pods {
		if terminating(pod) {
			continue
		}
		if old, ok := d.uids[name]; ok && old != uidOf(pod) && !slices.Contains(d.replaced, name) {
			d.replaced = append(d.replaced, name)
		}
	}
}

func downMembers(fleetName string, replicas int64, pods map[string]object) []string {
	var down []string
	for i := range replicas {
		name := fmt.Sprintf("%s-%d", fleetName, i)
		if pod, ok := pods[name]; !ok || !podReady(pod) {
			down = append(down, name)
		}
	}
	return down
}

// waitWatching polls check while enforcing the one-disruption invariant.
func (h *harness) waitWatching(description string, timeout time.Duration, d *disruptions, check func() bool) {
	h.waitFor(description, timeout, func() bool {
		h.observe(d)
		return check()
	})
	fmt.Printf("PASS: %s never had more than %d member(s) down; replaced in order %v\n", d.fleet, d.maxDown, d.replaced)
}

func (h *harness) fleetReason(fleetName string) (string, string) {
	c := condition(h.get("celldfleet", fleetName), "Ready")
	return str(c, "reason"), str(c, "message")
}

// killOperator force-deletes the running manager Pod, the way a node loss
// or OOM kill removes it, and waits for its replacement.
func (h *harness) killOperator() {
	for _, pod := range items(decode(h.k("-n", operatorNS, "get", "pods", "-l", "app.kubernetes.io/name=celld-operator,pod-template-hash", "-o", "json"))) {
		h.k("-n", operatorNS, "delete", "pod", nameOf(pod), "--grace-period=0", "--force", "--wait=false")
	}
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=180s")
	fmt.Println("Killed the manager Pod; its replacement is running")
}

func (h *harness) restartOperator() {
	h.k("-n", operatorNS, "rollout", "restart", "deployment/celld-operator")
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=120s")
}
