package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// exerciseOrderedBucket is the Ordered Bucket live qualification on the
// explicitly owned disposable cluster, whose third node sits in a second zone.
func (h *harness) exerciseOrderedBucket() {
	alpha := bucketFleet("alpha", "bucket-alpha")
	alpha.Spec.BucketWorkload = "Ordered"
	alpha.Spec.Replicas = 3
	alpha.Spec.Placement.AZCount = 2
	alpha.Spec.Placement.Zones = []string{"us-east-1a", "us-east-1b"}
	h.apply(alpha)
	pods := func() []object { return h.fleetPods("alpha") }
	settled := func(count int64) bool {
		current := h.get("celldfleet", "alpha")
		sts, err := h.tryGet("fleets", "statefulset", "alpha")
		if err != nil {
			if strings.Contains(err.Error(), "(NotFound)") {
				return false
			}
			must(err)
		}
		return specReplicas(sts) == count && num(sts, "status", "readyReplicas") == count && isReady(current) && operationID(current) == ""
	}
	placement := func(count int) {
		current := pods()
		assert(len(current) == count, "%d pods, expected %d", len(current), count)
		names := map[string]bool{}
		for _, pod := range current {
			names[nameOf(pod)] = true
		}
		expected := map[string]bool{}
		for i := range count {
			expected[fmt.Sprintf("alpha-%d", i)] = true
		}
		assert(same(names, expected), "membership %v", names)
		assert(len(nodeNames(current)) == count, "ordered replicas share a node")
		for _, pod := range current {
			ordinal, err := strconv.Atoi(nameOf(pod)[strings.LastIndex(nameOf(pod), "-")+1:])
			must(err)
			zone := []string{"us-east-1a", "us-east-1b"}[ordinal%2]
			node := h.cluster("node", str(pod, "spec", "nodeName"))
			assert(str(node, "metadata", "labels", "topology.kubernetes.io/zone") == zone, "%s scheduled outside %s", nameOf(pod), zone)
			assert(str(pod, "spec", "nodeSelector", "topology.kubernetes.io/zone") == zone, "%s selects the wrong zone", nameOf(pod))
			assert(len(list(pod, "spec", "schedulingGates")) == 0, "%s still gated", nameOf(pod))
		}
		assert(len(h.listIn("fleets", "pvc")) == 0, "Ordered Bucket created claims")
		fmt.Println("PASS: exact ordered membership, AZ mapping, hostname separation, and emptyDir")
	}
	scale := func(count int) {
		h.setReplicas("alpha", count)
		h.waitFor(fmt.Sprintf("Ordered Bucket settled at %d", count), 360*time.Second, func() bool { return settled(int64(count)) })
		placement(count)
	}
	h.waitFor("Ordered Bucket gated pods start across two AZs", 360*time.Second, func() bool { return settled(3) })
	placement(3)
	client := sleepPod("ordered-client", "fleets", curlImage, map[string]string{"celld.eric.dev/client-of": "alpha"})
	client.Spec.Containers[0].Name = "client"
	h.apply(client)
	h.k("-n", "fleets", "wait", "--for=condition=Ready", "pod/ordered-client", "--timeout=90s")
	request := func(method, path string) object {
		return decode(h.k("-n", "fleets", "exec", "ordered-client", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "10", "-X", method, "http://alpha:8080"+path))
	}
	paths := make([]string, 0, 12)
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/?cell=ordered&id=ack-%d", i))
	}
	for _, path := range paths {
		assert(stored(request("PUT", path)), "write %s not acknowledged", path)
	}
	original := nameUIDs(pods())
	scale(2)
	expected := map[string]string{}
	for key, value := range original {
		if key != "alpha-2" {
			expected[key] = value
		}
	}
	assert(same(nameUIDs(pods()), expected), "contraction replaced a surviving ordinal")
	for _, path := range paths {
		assert(stored(request("GET", path)), "write %s lost", path)
	}
	fmt.Println("PASS: only highest ordinal removed; all 12 acknowledged writes survived")
	// This node was admitted by completed removal. Restart its unchanged pinned
	// runtime in place, then require positive generation supersession on the next removal.
	before := h.get("pod", "alpha-0")
	oldContainer := str(list(before, "status", "containerStatuses")[0], "containerID")
	h.k("-n", "fleets", "exec", "alpha-0", "--", "/bin/sh", "-c", "kill -TERM 1")
	h.waitFor("same Pod UID acquires a new ready runtime invocation", 180*time.Second, func() bool {
		statuses := list(h.get("pod", "alpha-0"), "status", "containerStatuses")
		return len(statuses) > 0 && str(statuses[0], "containerID") != oldContainer && boolean(statuses[0], "ready")
	})
	assert(uidOf(h.get("pod", "alpha-0")) == uidOf(before), "in-place restart replaced the Pod")
	scale(3)
	assert(uidOf(h.get("pod", "alpha-2")) != original["alpha-2"], "readmitted ordinal reused the old Pod")
	h.restartOperator()
	scale(2)
	for _, path := range paths {
		assert(stored(request("GET", path)), "write %s lost", path)
	}
	fleetUID := uidOf(h.get("celldfleet", "alpha"))
	var journal object
	for _, reservation := range h.reservations() {
		if str(reservation, "spec", "fleetUID") == fleetUID {
			journal = decode(annotation(reservation, journalAnnotation))
		}
	}
	superseded := false
	for _, session := range list(journal, "BucketHistory") {
		if truthy(field(session, "SupersededBy")) {
			superseded = true
		}
	}
	assert(superseded, "no positive successor recorded in %v", list(journal, "BucketHistory"))
	fmt.Println("PASS: positive successor history survives manager restart and repeated contraction; 12/12 ledger intact")
	_, err := h.tryK("-n", "fleets", "exec", "ordered-client", "--", "curl", "--fail", "--silent", "--max-time", "3", "http://"+str(h.get("pod", "alpha-0"), "status", "podIP")+":8081/state")
	assert(err != nil, "application client reached private state endpoint")
	fmt.Println("PASS: private peer/state endpoint remains inaccessible to application clients")
}

func nameUIDs(pods []object) map[string]string {
	out := map[string]string{}
	for _, pod := range pods {
		out[nameOf(pod)] = uidOf(pod)
	}
	return out
}

// truthy mirrors Python truthiness for decoded JSON values.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case object:
		return len(v) > 0
	default:
		return true
	}
}
