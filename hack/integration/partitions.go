package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// exercisePartitions separates observation loss, peer isolation and object-store
// loss. Each fault is proved from a probe with the affected network identity,
// held across several reconciles, then removed. A transient partition must not
// erase a retained disk. Only responses that acknowledge an exact write enter
// the ledger, which is checked after each recovery.
func (h *harness) exercisePartitions() {
	h.scale("beta", 3)
	h.writeLedger("beta")
	f := h.get("celldfleet", "beta")
	uid := uidOf(f)
	peerProbe := "partition-peer"
	h.probe("partition-observer", operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	defer h.k("-n", operatorNS, "delete", "pod", "partition-observer", "--wait=false")
	// Exercise the real Collector during observation loss. Shadow runs the
	// same reads as Automatic without changing the requested replica count.
	h.merge("beta", `{"spec":{"capacity":{"mode":"Shadow","minReplicas":3,"maxReplicas":3,"sampleIntervalSeconds":15}}}`)
	h.waitCapacityCoverage(3)

	victim := "statefulset.kubernetes.io/pod-name == 'beta-1'"
	fleetSelector := fmt.Sprintf("celld.eric.dev/fleet-uid == '%s'", uid)
	peerRule := object{"action": "Deny", "protocol": "TCP", "destination": object{"ports": []int{8081}}}

	h.fault("operator cannot observe one healthy member", func() {
		r := request{pod: "partition-observer", ns: operatorNS, address: str(h.get("pod", "beta-1"), "status", "podIP")}
		h.curl(r)
		rule := object{"action": "Deny", "protocol": "TCP", "source": object{
			"namespaceSelector": "kubernetes.io/metadata.name == '" + operatorNS + "'",
			"selector":          "app.kubernetes.io/name == 'celld-operator'",
		}, "destination": object{"ports": []int{8081}}}
		restore := h.partition("observation", victim, []object{rule}, nil)
		defer restore()
		h.waitNetwork(r, false)
		// Calico can preserve established keep-alive streams. Restart the
		// observer under the active deny so its reads use new connections.
		h.killOperator()
		h.waitCapacityCoverage(2)
		h.holdPartition(30*time.Second, func() {
			h.observe(h.faultWatch)
			must(sameDisks(h.faultBefore.claims, h.claims("beta")))
			assert(same(h.faultBefore.pods, nameUIDs(h.memberPods("beta"))), "observation loss replaced a healthy member Pod")
		})
		restore()
		h.waitNetwork(r, true)
		h.waitCapacityCoverage(3)
	}, h.sameDisksAfter(), h.restartedInPlace())
	h.merge("beta", `{"spec":{"capacity":null}}`)
	// This peer probe carries the fleet label for network admission. Create
	// it after capacity collection is disabled: it is not a runtime member.
	pod := sleepPod(peerProbe, "fleets", map[string]string{"celld.eric.dev/fleet-uid": uid})
	pod.Spec.NodeName = h.nodes[0]
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "celld.eric.dev/v1alpha1", Kind: "CelldFleet", Name: "beta", UID: types.UID(uid), Controller: new(true)}}
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "integration.celld.eric.dev/NotServing"}}
	h.apply(pod)
	h.wait("partition peer probe running", func() bool { return str(h.get("pod", peerProbe), "status", "phase") == "Running" })
	defer h.k("-n", "fleets", "delete", "pod", peerProbe, "--wait=false")

	h.fault("one member isolated from peer RPC", func() {
		r := request{pod: peerProbe, address: str(h.get("pod", "beta-1"), "status", "podIP")}
		h.curl(r)
		ingress := object{"action": "Deny", "protocol": "TCP", "source": object{"selector": fleetSelector}, "destination": object{"ports": []int{8081}}}
		restore := h.partition("peers", victim, []object{ingress}, []object{peerRule})
		defer restore()
		h.waitNetwork(r, false)
		h.holdPartition(30*time.Second, func() {
			h.observe(h.faultWatch)
			must(sameDisks(h.faultBefore.claims, h.claims("beta")))
		})
		restore()
		h.waitNetwork(r, true)
	}, h.sameDisksAfter())

	h.fleetOutage("fleet loses S3 while the manager restarts", func() {
		r := request{pod: peerProbe, address: "minio." + storeNS + ".svc", path: "/minio/health/live", port: 9000}
		h.curl(r)
		rule := object{"action": "Deny", "protocol": "TCP", "destination": object{"ports": []int{9000}}}
		restore := h.partition("store", fleetSelector, nil, []object{rule})
		defer restore()
		h.waitNetwork(r, false)
		h.killOperator()
		h.holdPartition(30*time.Second, func() {
			must(sameDisks(h.faultBefore.claims, h.claims("beta")))
			assert(same(h.faultBefore.pods, nameUIDs(h.memberPods("beta"))), "transient store outage replaced a member Pod")
		})
		restore()
		h.waitNetwork(r, true)
	}, h.restartedInPlace())
	fmt.Println("PASS: observation, peer and S3 partitions recovered with retained disks and every acknowledged write readable")
}

func (h *harness) waitCapacityCoverage(count int64) {
	h.waitFor(fmt.Sprintf("beta capacity observation covers %d members", count), 2*time.Minute, func() bool {
		capacity := sub(h.get("celldfleet", "beta"), "status", "capacity")
		if str(capacity, "mode") != "Shadow" || num(capacity, "coveredReplicas") != count || num(capacity, "usefulReplicas") != count {
			return false
		}
		return count == 3 || str(capacity, "reason") == "PendingCapacity"
	})
}

// partition uses Calico's explicit Deny before the normal Kubernetes allow
// policies. An additional Kubernetes NetworkPolicy cannot deny traffic already
// allowed by another policy. The resource lives only in this disposable cluster.
// https://docs.tigera.io/calico/latest/reference/resources/networkpolicy
func (h *harness) partition(name, selector string, ingress, egress []object) func() {
	name = "integration-partition-" + name
	spec := object{"order": 0, "selector": selector}
	var directions []string
	if len(ingress) > 0 {
		spec["ingress"] = ingress
		directions = append(directions, "Ingress")
	}
	if len(egress) > 0 {
		spec["egress"] = egress
		directions = append(directions, "Egress")
	}
	spec["types"] = directions
	h.apply(object{"apiVersion": "crd.projectcalico.org/v1", "kind": "NetworkPolicy", "metadata": object{"name": name, "namespace": "fleets"}, "spec": spec})
	restored := false
	return func() {
		if !restored {
			h.k("-n", "fleets", "delete", "networkpolicies.crd.projectcalico.org", name, "--ignore-not-found")
			restored = true
		}
	}
}

// waitNetwork requires curl itself to prove denial. A missing/failed probe,
// forbidden exec or a Kubernetes API error must not be counted as a partition.
func (h *harness) waitNetwork(r request, reachable bool) {
	if r.ns == "" {
		r.ns = "fleets"
	}
	if r.port == 0 {
		r.port = 8081
	}
	if r.path == "" {
		r.path = "/state"
	}
	h.waitFor(fmt.Sprintf("%s -> %s:%d reachable=%v", r.pod, r.address, r.port, reachable), time.Minute, func() bool {
		out := h.run(command{args: h.kubectl("-n", r.ns, "exec", r.pod, "--", "/bin/sh", "-c",
			fmt.Sprintf("curl --silent --output /dev/null --connect-timeout 2 --max-time 3 'http://%s:%d%s'; echo CURL_EXIT:$?", r.address, r.port, r.path)), timeout: 15 * time.Second})
		code, ok := curlExit(out)
		assert(ok, "network probe did not report a curl result: %s", out)
		if reachable {
			return code == 0
		}
		return code == 7 || code == 28
	})
}

func (h *harness) holdPartition(duration time.Duration, check func()) {
	until := time.Now().Add(duration)
	for time.Now().Before(until) {
		check()
		select {
		case <-h.ctx.Done():
			fail("integration interrupted during partition")
		case <-time.After(2 * time.Second):
		}
	}
}

func curlExit(out string) (int, bool) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(out), "CURL_EXIT:")
	if !ok {
		return 0, false
	}
	code, err := strconv.Atoi(raw)
	return code, err == nil && code >= 0 && code <= 255
}
