package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ewhauser/celld-operator/api/v1alpha1"
)

// extendedBuckets are the buckets the extended suite's own fleets use.
var extendedBuckets = []string{"bucket-five", "bucket-pair", "bucket-mesh", "bucket-meshb"}

// extendedImage is the runtime a bucket's application is deployed with. The
// admission fleets start on the upgrade source when there is one, so their
// runtime upgrade runs with injected Pods.
func (h *harness) extendedImage(bucket string) string {
	if strings.HasPrefix(bucket, "bucket-mesh") && h.upgrades() {
		return h.opts.upgradeFrom
	}
	return h.opts.runtimeImage
}

// smallFleet sizes a fleet for the CI runner: the extended suite runs up to
// eleven members at once on three kind nodes.
func (h *harness) smallFleet(name, bucket, profile string, replicas int32) *v1alpha1.CelldFleet {
	f := h.newFleet(name, bucket, profile, "fleets")
	f.Spec.Replicas = replicas
	f.Spec.Execution = &v1alpha1.ExecutionSpec{CPURequest: "50m", MemoryRequest: "128Mi", MemoryLimit: "512Mi"}
	return f
}

// exerciseExtended covers what the other suites cannot on their fixtures
// (ADR 0023 qualification): a five-member PersistentFleet, loss of the last
// copy of a session, a node drain racing a rolling update, and admission
// webhooks that mutate fleet Pods.
//
// CELLD_EXTENDED_SCENARIOS (comma-separated: five, loss, drain, admission)
// runs a subset when iterating locally; unset runs all four.
func (h *harness) exerciseExtended() {
	selected := strings.Split(os.Getenv("CELLD_EXTENDED_SCENARIOS"), ",")
	runs := func(name string) bool {
		if len(selected) == 1 && selected[0] == "" {
			return true
		}
		return slices.Contains(selected, name)
	}
	if runs("five") {
		h.exerciseFiveMembers()
	}
	if runs("loss") {
		h.exerciseLastCopyLoss()
	}
	if runs("drain") {
		h.exerciseDrainDuringRollout()
	}
	if runs("admission") {
		// The admission fleets need the room.
		h.deleteFleet("beta")
		h.deleteFleet("alpha")
		h.exerciseAdmission()
	}
}

// checkpoint verifies the ledger mid-scenario: the writer's acknowledged
// writes so far are recorded and read back, and a fresh writer resumes.
func (h *harness) checkpoint(fleetName string) {
	h.stopWriter(fleetName)
	h.readLedger(fleetName)
	h.startWriter(fleetName)
}

// exerciseFiveMembers runs a five-member PersistentFleet on three nodes
// (Relaxed placement) through a rolling restart, contraction to three and
// regrowth, all under continuous writes. With five members every leader
// keeps two followers.
func (h *harness) exerciseFiveMembers() {
	fmt.Println("EXTENDED: five-member PersistentFleet")
	five := h.smallFleet("five", "bucket-five", "PersistentFleet", 5)
	five.Spec.Placement.Mode = "Relaxed"
	h.apply(five)
	h.waitSettled("five", 5, 15*time.Minute)
	placement := map[string]string{}
	for _, pod := range h.memberPods("five") {
		placement[nameOf(pod)] = str(pod, "spec", "nodeName")
	}
	fmt.Println("Five members on three nodes:", placement)
	h.assertFollowers("five", 2)
	original := h.claims("five")
	h.writeLedger("five")
	h.startWriter("five")

	h.rollAll("five", encode(object{"spec": object{"maintenance": object{"restartToken": "extended-five"}}}), nil, nil)
	h.checkpoint("five")

	for _, target := range []int{4, 3} {
		before := h.claims("five")
		removed := fmt.Sprintf("data-five-%d", target)
		h.shrinkWatchingRelease("five", target, nil, nil)
		after := h.claims("five")
		_, kept := after[removed]
		assert(!kept, "contraction to %d kept %s", target, removed)
		for name, id := range after {
			assert(before[name] == id, "contraction to %d changed surviving disk %s", target, name)
		}
		h.goneVolumes(map[string]claimIdentity{removed: before[removed]})
		h.checkpoint("five")
	}

	h.growWatching("five", 5)
	grown := h.claims("five")
	for name, id := range grown {
		if ordinal, _ := claimOrdinal("five", name); ordinal >= 3 {
			assert(freshDisk(original[name], id), "regrowth reused disk %s", name)
		} else {
			assert(original[name] == id, "regrowth changed disk %s", name)
		}
	}
	h.assertFollowers("five", 2)
	h.stopWriter("five")
	h.readLedger("five")
	h.deleteFleet("five")
	fmt.Println("PASS: five-member PersistentFleet rolled, contracted 5 -> 3 and regrew with two followers per leader and every acknowledged write readable")
}

// growWatching raises a PersistentFleet's replicas and waits for it to settle.
// Growth adds members together, so only the existing members are required to
// stay up throughout.
func (h *harness) growWatching(fleetName string, target int) {
	from := specReplicas(h.get("statefulset", fleetName))
	h.setReplicas(fleetName, target)
	h.waitFor(fmt.Sprintf("%s grows %d -> %d with existing members up", fleetName, from, target), 10*time.Minute, func() bool {
		pods := map[string]object{}
		for _, pod := range h.memberPods(fleetName) {
			pods[nameOf(pod)] = pod
		}
		down := downMembers(fleetName, from, pods)
		assert(len(down) <= 1, "growth disrupted existing %s members: %v", fleetName, down)
		return h.settled(fleetName, int64(target))
	})
}

// nodeLogReader is a probe with the operator's network identity, the only
// client the fleet NetworkPolicy admits to a member's internal /state.
const nodeLogReader = "node-log-reader"

// nodeLogs reads /state.node_log from every ready member, keyed by Pod name.
// A member that does not answer is omitted.
// nodeLogsWithErrors also reports why a member's node-log state was not read,
// so a timed-out assertion can say whether a member was unreadable or short.
func (h *harness) nodeLogsWithErrors(fleetName string) (map[string]object, map[string]string) {
	out, errs := map[string]object{}, map[string]string{}
	for _, pod := range h.memberPods(fleetName) {
		ip := str(pod, "status", "podIP")
		if !podReady(pod) || ip == "" {
			errs[nameOf(pod)] = "not ready"
			continue
		}
		body, err := h.try(command{args: h.kubectl("-n", operatorNS, "exec", nodeLogReader, "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "3", "http://"+ip+":8081/state"), timeout: 20 * time.Second})
		if err != nil {
			errs[nameOf(pod)] = strings.TrimSpace(err.Error())
			continue
		}
		var state object
		if json.Unmarshal([]byte(body), &state) != nil || sub(state, "node_log") == nil {
			errs[nameOf(pod)] = "no node_log in /state"
			continue
		}
		out[nameOf(pod)] = sub(state, "node_log")
	}
	return out, errs
}

// assertFollowers waits until every member of a settled fleet reports fleet
// posture and an own ensemble of exactly want followers, none of them itself.
func (h *harness) assertFollowers(fleetName string, want int) {
	h.probe(nodeLogReader, operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	// The probe matches the manager Deployment's selector; it must not
	// outlive this check.
	defer h.k("-n", operatorNS, "delete", "pod", nodeLogReader, "--wait=true")
	count := specReplicas(h.get("celldfleet", fleetName))
	var last map[string][]string
	var unread map[string]string
	description := fmt.Sprintf("every %s leader reports %d follower(s)", fleetName, want)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		logs, errs := h.nodeLogsWithErrors(fleetName)
		last, unread = map[string][]string{}, errs
		settled := int64(len(logs)) == count
		for node, log := range logs {
			ensemble := strs(log, "own", "ensemble")
			last[node] = ensemble
			if str(log, "posture") != "fleet" || len(ensemble) != want || slices.Contains(ensemble, node) {
				settled = false
			}
		}
		if settled {
			fmt.Println("PASS:", description)
			break
		}
		if !time.Now().Before(deadline) {
			fail("Timed out: %s; ensembles %v; unread %v", description, last, unread)
		}
		h.sleep(2 * time.Second)
	}
	fmt.Printf("PASS: %s ensembles %v\n", fleetName, last)
}

// exerciseLastCopyLoss destroys both disks of a two-member PersistentFleet at
// once. Each leader's only follower is the other member, so a session whose
// tail was not yet in the bucket loses its last complete copy. The operator
// must replace both members without waiting, and celld must record any loss
// instead of hanging or losing writes silently.
func (h *harness) exerciseLastCopyLoss() {
	fmt.Println("EXTENDED: last complete copy of a session lost")
	pair := h.smallFleet("pair", "bucket-pair", "PersistentFleet", 2)
	h.apply(pair)
	h.waitSettled("pair", 2, 10*time.Minute)
	h.assertFollowers("pair", 1)
	h.writeLedger("pair")
	h.startWriter("pair")
	h.sleep(30 * time.Second)
	h.writeLedger("pair")
	h.stopWriter("pair")
	acked := len(h.ledgers["pair"])
	before := h.claims("pair")

	start := time.Now()
	h.destroyDisks("pair", "pair-0", "pair-1")
	h.waitFor("operator replaces both lost disks without waiting", 15*time.Minute, func() bool {
		after := h.claims("pair")
		return after["data-pair-0"].UID != before["data-pair-0"].UID && after["data-pair-1"].UID != before["data-pair-1"].UID
	})
	h.waitSettled("pair", 2, 15*time.Minute-time.Since(start))
	fmt.Printf("PASS: pair recovered from losing both disks in %s\n", time.Since(start).Round(time.Second))
	after := h.claims("pair")
	for _, name := range []string{"data-pair-0", "data-pair-1"} {
		assert(freshDisk(before[name], after[name]), "%s did not come back on a fresh disk", name)
	}
	lost := h.warningEvents("pair", "MemberDiskLost")
	assert(len(lost) > 0, "no MemberDiskLost event for pair")
	fmt.Printf("PASS: %d MemberDiskLost event(s): %s\n", len(lost), strings.Join(lost, " | "))

	readable, missing := h.sweepLedger("pair", 90*time.Second)
	records := h.lossRecords("bucket-pair")
	fmt.Printf("Last-copy loss: %d acknowledged, %d readable, %d not readable, %d loss record(s) %v\n", acked, readable, len(missing), len(records), records)
	if len(missing) == 0 {
		fmt.Println("PASS: no acknowledged write was lost; every one read back after both disks were destroyed")
	} else {
		assert(len(records) > 0, "%d acknowledged write(s) lost with no loss record in bucket-pair: %v", len(missing), missing)
		fmt.Printf("PASS: %d acknowledged write(s) lost, and the loss is recorded in %d loss record(s)\n", len(missing), len(records))
	}

	// The recovered fleet serves new writes, including to cells that lost data.
	h.ledgers["pair"] = nil
	h.writeLedger("pair")
	h.startWriter("pair")
	h.sleep(20 * time.Second)
	h.stopWriter("pair")
	h.readLedger("pair")
	h.deleteFleet("pair")
	fmt.Println("PASS: losing the last copy of a session never hangs the operator and is never silent")
}

// destroyDisks makes every named member's disk disappear at once, as a
// backend losing volumes would: celld is killed without a drain, the PVs are
// removed without their finalizers, and the Pods are force-deleted. The old
// directories stay on the nodes but no claim can reach them again.
func (h *harness) destroyDisks(fleetName string, members ...string) {
	volumes := make([]string, 0, len(members))
	for _, member := range members {
		volumes = append(volumes, str(h.get("pvc", "data-"+member), "spec", "volumeName"))
	}
	for _, member := range members {
		h.sigkill(member)
	}
	h.k(append([]string{"delete", "pv", "--wait=false"}, volumes...)...)
	for _, pv := range volumes {
		h.k("patch", "pv", pv, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
	}
	h.k(append([]string{"-n", "fleets", "delete", "pod", "--force", "--grace-period=0", "--wait=false"}, members...)...)
	fmt.Println("Destroyed the disks of", fleetName, members, "at once")
}

// warningEvents returns the notes of Warning events with reason about a fleet.
func (h *harness) warningEvents(fleetName, reason string) []string {
	var out []string
	for _, e := range items(decode(h.k("-n", "fleets", "get", "events", "-o", "json"))) {
		about := str(e, "involvedObject", "name")
		if about == "" {
			about = str(e, "regarding", "name")
		}
		if about == fleetName && str(e, "reason") == reason && str(e, "type") == "Warning" {
			note := str(e, "message")
			if note == "" {
				note = str(e, "note")
			}
			out = append(out, note)
		}
	}
	return out
}

var lossRecord = regexp.MustCompile(`(\S*log/\S+\.e\d+\.loss\.json)\s*$`)

// lossRecords lists celld's loss records (log/<session>.e<epoch>.loss.json)
// in a bucket.
func (h *harness) lossRecords(bucket string) []string {
	name := fmt.Sprintf("loss-list-%d", time.Now().UnixNano())
	h.k("-n", storeNS, "run", name, "--restart=Never", "--image="+mcImage, "--command", "--", "/bin/sh", "-c",
		"mc alias set local http://minio:9000 qualification qualification-only >/dev/null && mc ls --recursive local/"+bucket)
	h.waitFor("listing "+bucket, 2*time.Minute, func() bool { return h.succeeded(name) })
	listing := h.k("-n", storeNS, "logs", name)
	h.k("-n", storeNS, "delete", "pod", name, "--wait=false")
	return parseLossRecords(listing)
}

func parseLossRecords(listing string) []string {
	var out []string
	for line := range strings.SplitSeq(listing, "\n") {
		if m := lossRecord.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// exerciseDrainDuringRollout drains the node of a not-yet-updated member
// while a rolling restart is in progress. The PDB refuses the eviction while
// another member is down and lets it through only when the fleet is settled.
// Strict host separation keeps the drained member off every other node, so
// the rollout waits until the node is uncordoned, then completes on retained
// disks.
func (h *harness) exerciseDrainDuringRollout() {
	fmt.Println("EXTENDED: node drain during a rolling restart")
	h.scale("beta", 3)
	h.writeLedger("beta")
	h.startWriter("beta")
	disks := h.claims("beta")
	before := nameUIDs(h.memberPods("beta"))
	oldRevision := str(h.get("statefulset", "beta"), "status", "updateRevision")
	target := "beta-0"
	node := str(h.get("pod", target), "spec", "nodeName")
	defer h.k("uncordon", node)
	d := h.watchDisruptions("beta")
	h.merge("beta", encode(object{"spec": object{"maintenance": object{"restartToken": "extended-drain"}}}))
	h.waitFor("rolling restart under way with the PDB closed", 5*time.Minute, func() bool {
		h.observe(d)
		pod := h.get("pod", target)
		assert(uidOf(pod) == before[target], "%s was replaced before the drain began", target)
		return len(d.replaced) > 0 && h.budget("beta") == 0
	})

	type result struct {
		out string
		err error
	}
	drained := make(chan result, 1)
	selector := "--pod-selector=celld.eric.dev/fleet-uid=" + uidOf(h.get("celldfleet", "beta"))
	go func() {
		out, err := h.try(command{args: h.kubectl("drain", node, selector, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=900s"), timeout: 16 * time.Minute})
		drained <- result{out, err}
	}()
	var drain result
	h.waitWatching("drain of "+node+" completes under the PDB", 16*time.Minute, d, func() bool {
		select {
		case drain = <-drained:
			return true
		default:
			return false
		}
	})
	must(drain.err)
	assert(strings.Contains(drain.out, "disruption budget"), "drain was never refused by the PDB:\n%s", drain.out)
	fmt.Println("PASS: the PDB refused the drain's eviction while another member was down and admitted it once settled")

	// The drained member cannot come back while its node is cordoned; the
	// fleet stays unsettled and no second member is disrupted.
	h.hold(20*time.Second, target+" stays down on its cordoned node and the PDB stays closed", func() bool {
		h.observe(d)
		pod, err := h.tryGet("fleets", "pod", target)
		return (err != nil || !podReady(pod)) && h.budget("beta") == 0
	})
	h.k("uncordon", node)
	h.waitWatching("rolling restart completes after uncordon", 20*time.Minute, d, func() bool {
		after := nameUIDs(h.memberPods("beta"))
		for name, uid := range before {
			if after[name] == uid {
				return false
			}
		}
		return h.settled("beta", 3)
	})
	revision := str(h.get("statefulset", "beta"), "status", "updateRevision")
	assert(revision != oldRevision, "restart token did not produce a new revision")
	for _, pod := range h.memberPods("beta") {
		assert(str(pod, "metadata", "labels", "controller-revision-hash") == revision, "%s is not on the update revision", nameOf(pod))
	}
	must(sameDisks(disks, h.claims("beta")))
	h.stopWriter("beta")
	h.readLedger("beta")
	fmt.Println("PASS: a node drain during a rolling restart was serialized by the PDB; the rollout completed on retained disks")
}
