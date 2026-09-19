package main

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const automaticCapacity = `{"mode":"Automatic","minReplicas":%d,"maxReplicas":3,"minSamples":2,"sampleIntervalSeconds":30,"scaleInStabilizationSeconds":60,"scaleInCooldownSeconds":60,"cpuLowMillicores":200,"cpuHighMillicores":500,"memoryLowMiB":700,"memoryHighMiB":1000}`

// workloadKind is the workload behind a fleet: Deployment for Bucket, StatefulSet otherwise.
func (h *harness) workloadKind(fleetName string) string {
	if str(h.get("celldfleet", fleetName), "spec", "profile") == "Bucket" {
		return "deployment"
	}
	return "statefulset"
}

// ready reports a fleet whose Ready condition is true and whose workload has
// observed its latest generation with every replica ready.
func (h *harness) ready(fleetName string) bool {
	current := h.get("celldfleet", fleetName)
	observed := h.get(h.workloadKind(fleetName), fleetName)
	observedGeneration := num(observed, "status", "observedGeneration")
	generation := generation(observed)
	if generation == 0 {
		generation = 1
	}
	return isReady(current) && observedGeneration >= generation && num(observed, "status", "readyReplicas") == specReplicas(observed)
}

// noOperation reports that no lifecycle operation is currently recorded in status.
func (h *harness) noOperation(fleetName string) bool {
	return operationID(h.get("celldfleet", fleetName)) == ""
}

func (h *harness) exerciseIsolation() {
	alpha := bucketFleet("alpha", "bucket-alpha")
	beta := newFleet("beta", "bucket-beta", "PersistentFleet", "fleets")
	// An old deterministic claim must never be adopted into a new bucket.
	h.apply(&corev1.PersistentVolumeClaim{
		APIVersion: "v1", Kind: "PersistentVolumeClaim",
		Name: "data-collision-0", Namespace: "fleets", Labels: map[string]string{"celld.example.com/fleet-uid": "previous-fleet"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: new("retained"),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	})
	oldClaimUID := uidOf(h.get("pvc", "data-collision-0"))
	h.apply(newFleet("collision", "new-bucket", "PersistentFleet", "fleets"))
	h.wait("pre-existing PVC blocks initial StatefulSet creation", func() bool {
		return hasReason(h.get("celldfleet", "collision"), "StorageIdentityConflict")
	})
	assert(strings.TrimSpace(h.k("-n", "fleets", "get", "statefulset", "collision", "--ignore-not-found", "-o", "name")) == "", "collision StatefulSet was created")
	assert(uidOf(h.get("pvc", "data-collision-0")) == oldClaimUID, "pre-existing claim was replaced")
	h.apply(alpha)
	h.apply(beta)
	h.waitFor("both runtime profiles ready through operator reconciliation", 300*time.Second, func() bool {
		return h.ready("alpha") && h.ready("beta")
	})
	pods := h.listIn("fleets", "pods")
	addresses := map[string]string{}
	for _, fleetName := range []string{"alpha", "beta"} {
		uid := uidOf(h.get("celldfleet", fleetName))
		var selected []object
		for _, pod := range pods {
			if str(pod, "metadata", "labels", "celld.example.com/fleet-uid") == uid {
				selected = append(selected, pod)
			}
		}
		assert(len(selected) == 2, "%s has %d pods", fleetName, len(selected))
		assert(len(nodeNames(selected)) == 2, "%s replicas share a node", fleetName)
		addresses[fleetName] = str(selected[0], "status", "podIP")
	}
	fmt.Println("PASS: strict replicas use distinct nodes within the configured zone")
	unavailable := bucketFleet("unavailable", "bucket-unavailable")
	unavailable.Spec.Placement.AZCount = 2
	unavailable.Spec.Placement.Zones = []string{"us-east-1a", "us-east-1b"}
	h.apply(unavailable)
	h.wait("strict placement blocks a missing eligible zone", func() bool {
		for _, pod := range h.fleetPods("unavailable") {
			for _, c := range conditions(pod) {
				if str(c, "reason") == "Unschedulable" && strings.Contains(str(c, "message"), "topology spread") {
					return true
				}
			}
		}
		return false
	})
	alphaUID := uidOf(h.get("celldfleet", "alpha"))
	h.probe("same-fleet", "fleets", map[string]string{"celld.example.com/fleet-uid": alphaUID})
	h.probe("client", "fleets", map[string]string{"celld.example.com/client-of": "alpha"})
	h.probe("client-beta", "fleets", map[string]string{"celld.example.com/client-of": "beta"})
	h.probe("untrusted", "fleets", map[string]string{})
	h.probe("operator", operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	h.curl(request{pod: "same-fleet", address: addresses["alpha"]})
	h.curl(request{pod: "same-fleet", address: addresses["beta"], denied: true})
	h.curl(request{pod: "untrusted", address: addresses["alpha"], denied: true})
	h.curl(request{pod: "client", address: addresses["alpha"], denied: true})
	h.curl(request{pod: "operator", address: addresses["alpha"], ns: operatorNS})
	h.curl(request{pod: "client", address: "alpha", path: "/.well-known/celld/health", port: 8080})
	h.curl(request{pod: "client", address: "beta", path: "/.well-known/celld/health", port: 8080, denied: true})
	assert(stored(h.app("client", "alpha", "PUT", "/?cell=integration&id=ack")), "write not acknowledged")
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable")
	assert(!h.ackStored("client-beta", "beta"), "write visible from another fleet's storage scope")
	fmt.Println("PASS: application writes readable only in their own fleet storage scope")
	fmt.Println("PASS: same-fleet/operator peer access, cross-fleet/untrusted/client peer denial, ClusterIP health routing")
	h.k("-n", "fleets", "delete", "pod", "same-fleet", "--wait=true")
	h.apply(newFleet("conflict", "bucket-alpha", "Bucket", "other"))
	h.wait("cross-namespace storage conflict blocked", func() bool {
		return hasReason(h.getIn("other", "celldfleet", "conflict"), "StorageScopeConflict")
	})
	h.apply(newFleet("denied", "bucket-denied", "Bucket", "unbound"))
	h.wait("fleet in a namespace without the fleet Role is reported, not reconciled", func() bool {
		return hasReason(h.getIn("unbound", "celldfleet", "denied"), "NamespaceAccessDenied")
	})
	assert(strings.TrimSpace(h.k("-n", "unbound", "get", "deployment,service,networkpolicy", "-o", "name")) == "", "unbound namespace was reconciled")
	for _, mutate := range []string{"upgrade", "invalid-az", "invalid-storage"} {
		bad := alpha.DeepCopy()
		switch mutate {
		case "upgrade":
			bad.Spec.Storage.SizeGiB = 2
		case "invalid-az":
			bad.Name = "bad-az"
			bad.Spec.Placement.AZCount = 3
		case "invalid-storage":
			bad.Name = "bad-storage"
			bad.Spec.Profile = "PersistentFleet"
		}
		err := h.tryApply(bad)
		assert(err != nil, "admission accepted %s", mutate)
		assert(strings.Contains(strings.ToLower(err.Error()), "invalid"), "%v", err)
	}
	fmt.Println("PASS: schema rejects invalid specs and lifecycle mutations")
	// OnDelete keeps runtime Pods unchanged while testing drift detection.
	originalTemplate := sub(h.get("statefulset", "beta"), "spec", "template")
	betaPodUIDs := h.ordinalUIDs("beta", 2)
	h.k("-n", "fleets", "patch", "statefulset", "beta", "--type=json", "-p", encode([]object{
		{"op": "add", "path": "/spec/template/spec/containers/0/livenessProbe", "value": object{"exec": object{"command": []string{"false"}}}},
		{"op": "add", "path": "/spec/template/spec/containers/0/env/-", "value": object{"name": "CELLD_BUCKET", "value": "s3://other-fleet"}},
	}))
	h.wait("additional probe and bucket override are reported as unsafe drift", func() bool {
		c := condition(h.get("celldfleet", "beta"), "Ready")
		return c != nil && str(c, "reason") == "LifecycleBlocked"
	})
	assert(same(h.ordinalUIDs("beta", 2), betaPodUIDs), "drift detection replaced runtime pods")
	h.k("-n", "fleets", "patch", "statefulset", "beta", "--type=json", "-p", encode([]object{{"op": "replace", "path": "/spec/template", "value": originalTemplate}}))
	h.wait("restored original template matches normalized API defaults", func() bool { return h.ready("beta") })
}

func (h *harness) ordinalUIDs(fleetName string, count int) []string {
	uids := make([]string, 0, count)
	for i := range count {
		uids = append(uids, uidOf(h.get("pod", fmt.Sprintf("%s-%d", fleetName, i))))
	}
	return uids
}

func (h *harness) exercisePersistentLifecycle() {
	// Local-path RWO is deliberately local-test-only: test same-host
	// locks and scheduling, not CSI/RWOP/EBS attachment guarantees.
	h.probe("beta-client", "fleets", map[string]string{"celld.example.com/client-of": "beta"})
	h.app("beta-client", "beta", "PUT", "/?cell=integration&id=ack")
	settled := func(count int64) bool {
		return specReplicas(h.get("statefulset", "beta")) == count && h.ready("beta") && h.noOperation("beta")
	}
	if h.opts.rwopCSI {
		for i := range 2 {
			claim := h.get("pvc", fmt.Sprintf("data-beta-%d", i))
			modes := strs(claim, "spec", "accessModes")
			assert(same(modes, []string{"ReadWriteOncePod"}), "%v", modes)
			volume := h.cluster("pv", str(claim, "spec", "volumeName"))
			assert(str(volume, "spec", "csi", "driver") == "hostpath.csi.k8s.io", "%v", sub(volume, "spec"))
		}
		fmt.Println("PASS: PersistentFleet claims are ReadWriteOncePod on per-node CSI volumes")
	}
	for cycle := range 2 {
		h.setReplicas("beta", 3)
		h.waitFor(fmt.Sprintf("PersistentFleet growth %d", cycle), 300*time.Second, func() bool { return settled(3) })
		retainedUID := uidOf(h.get("pvc", "data-beta-2"))
		retainedHost := str(h.get("pod", "beta-2"), "spec", "nodeName")
		h.setReplicas("beta", 2)
		h.restartOperator()
		h.waitFor(fmt.Sprintf("PersistentFleet graceful retirement %d", cycle), 300*time.Second, func() bool { return settled(2) })
		assert(uidOf(h.get("pvc", "data-beta-2")) == retainedUID, "retired claim was replaced")
		assert(h.ackStored("beta-client", "beta"), "acknowledged write unreadable after retirement")
		h.setReplicas("beta", 3)
		h.waitFor(fmt.Sprintf("PersistentFleet same-host reactivation %d", cycle), 300*time.Second, func() bool { return settled(3) })
		assert(uidOf(h.get("pvc", "data-beta-2")) == retainedUID, "reactivated claim was replaced")
		assert(str(h.get("pod", "beta-2"), "spec", "nodeName") == retainedHost, "reactivated ordinal moved hosts")
	}
	h.merge("beta", `{"spec":{"capacity":`+fmt.Sprintf(automaticCapacity, 2)+`}}`)
	h.waitFor("PersistentFleet live automatic contraction", 420*time.Second, func() bool { return settled(2) })
	assert(h.ackStored("beta-client", "beta"), "acknowledged write unreadable after automatic contraction")
	fmt.Println(h.k("get", "celldstoragereservations", "-o", "json"))
}

// exerciseAdditiveCapacity checks manual additive capacity through the durable
// journal, across a leader restart, for both profiles.
func (h *harness) exerciseAdditiveCapacity() {
	for _, fleetName := range []string{"alpha", "beta"} {
		kind := h.workloadKind(fleetName)
		before := h.get(kind, fleetName)
		beforeTemplate := sub(before, "spec", "template")
		h.setReplicas(fleetName, 3)
		h.restartOperator()
		h.waitFor("journaled scale-out after controller restart: "+fleetName, 300*time.Second, func() bool {
			return specReplicas(h.get(kind, fleetName)) == 3 && h.ready(fleetName)
		})
		after := h.get(kind, fleetName)
		assert(len(nodeNames(h.fleetPods(fleetName))) == 3, "scaled replicas share a node")
		assert(uidOf(after) == uidOf(before), "workload was recreated")
		assert(same(sub(after, "spec", "template"), beforeTemplate), "scale-out changed the template")
		h.setReplicas(fleetName, 1)
		if h.bucketLifecycle && fleetName == "alpha" {
			h.waitFor("two actual Bucket decrements completed", 300*time.Second, func() bool {
				return specReplicas(h.get(kind, fleetName)) == 1 && h.ready(fleetName) && h.noOperation(fleetName)
			})
			assert(h.ackStored("client", "alpha"), "acknowledged write unreadable after contraction")
			h.setReplicas(fleetName, 3)
			h.waitFor("Bucket shrink/grow with retained generation history", 240*time.Second, func() bool {
				return specReplicas(h.get(kind, fleetName)) == 3 && h.ready(fleetName)
			})
			continue
		}
		reasons := []string{"BucketCompletionUnqualified"}
		if fleetName != "alpha" {
			reasons = []string{"FencingUnqualified"}
			if h.bucketLifecycle {
				reasons = append(reasons, "SessionBindingUnqualified", "HistoricalSessionUnresolved", "CapacityUncertain", "RecoveryInventoryUnavailable")
			}
		}
		h.wait("unqualified contraction blocked: "+fleetName, func() bool {
			return hasReason(h.get("celldfleet", fleetName), reasons...)
		})
		assert(specReplicas(h.get(kind, fleetName)) == 3, "unqualified contraction changed replicas")
		h.setReplicas(fleetName, 3)
		h.wait("restored desired capacity: "+fleetName, func() bool { return h.ready(fleetName) })
	}
	fmt.Println("PASS: both profiles scale out without template changes; contraction gates persist")
}

func (h *harness) exerciseBucketLifecycle() {
	// A paused unissued removal cannot change replicas. Its later
	// completion uses the same recorded membership checks.
	h.merge("alpha", `{"spec":{"maintenance":{"paused":true},"replicas":2}}`)
	h.wait("Bucket pause fence installed", func() bool {
		return annotation(h.get("deployment", "alpha"), "celld.example.com/maintenance-fence") == "paused"
	})
	h.sleep(12 * time.Second)
	assert(specReplicas(h.get("deployment", "alpha")) == 3, "paused removal changed replicas")
	h.merge("alpha", `{"spec":{"maintenance":null}}`)
	h.restartOperator()
	h.waitFor("Bucket resume and manager restart completes removal", 240*time.Second, func() bool {
		return specReplicas(h.get("deployment", "alpha")) == 2 && h.ready("alpha") && h.noOperation("alpha")
	})
	h.setReplicas("alpha", 3)
	h.waitFor("second Bucket growth", 240*time.Second, func() bool {
		return specReplicas(h.get("deployment", "alpha")) == 3 && h.ready("alpha")
	})
	h.merge("alpha", `{"spec":{"capacity":`+fmt.Sprintf(automaticCapacity, 1)+`}}`)
	h.waitFor("live Metrics Server automatic Bucket 3-to-1 contraction", 420*time.Second, func() bool {
		return specReplicas(h.get("deployment", "alpha")) == 1 && h.ready("alpha") && h.noOperation("alpha")
	})
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable after automatic contraction")
	retired := num(h.get("celldfleet", "alpha"), "status", "lifecycle", "retiredBucketSessions")
	assert(retired >= 5, "only %d retired sessions", retired)
	fmt.Println("PASS: acknowledged write preserved through repeated manual/automatic Bucket shrink-grow; retired sessions report unknown physical liveness:", retired)
	fmt.Println(h.k("get", "celldstoragereservations", "-o", "json"))
}

// exerciseBaseRemainder covers the native-manager path: capacity defaults with
// no Metrics Server, maintenance guards, blocked deletion, and restart safety.
func (h *harness) exerciseBaseRemainder() {
	// No Metrics Server is installed in this isolated cluster. Verify the
	// real API defaults/validation, exact metrics RBAC, and conservative
	// missing-data path in both profiles. Native HTTP fixture tests cover
	// successful /state + Metrics Server transport and pod incarnation races.
	for _, fleetName := range []string{"alpha", "beta"} {
		kind := h.workloadKind(fleetName)
		before := h.get(kind, fleetName)
		h.merge(fleetName, `{"spec":{"capacity":{}}}`)
		h.wait("capacity defaults to shadow: "+fleetName, func() bool {
			return str(h.get("celldfleet", fleetName), "status", "capacity", "mode") == "Shadow"
		})
		assert(same(sub(h.get(kind, fleetName), "spec"), sub(before, "spec")), "shadow capacity changed the workload")
		assert(num(h.get("celldfleet", fleetName), "spec", "capacity", "maxReplicas") == 10, "maxReplicas default missing")
		err := h.tryMerge("celldfleet", fleetName, `{"spec":{"capacity":{"minReplicas":10,"maxReplicas":3}}}`)
		assert(err != nil, "admission accepted inverted capacity bounds")
		assert(strings.Contains(strings.ToLower(err.Error()), "invalid"), "%v", err)
		h.merge(fleetName, `{"spec":{"capacity":{"mode":"ScaleOut"}}}`)
		h.wait("missing metrics block automatic capacity: "+fleetName, func() bool {
			reason := str(h.get("celldfleet", fleetName), "status", "capacity", "reason")
			return reason == "PendingCapacity" || reason == "IncompleteMetrics"
		})
		assert(same(sub(h.get(kind, fleetName), "spec"), sub(before, "spec")), "missing metrics changed the workload")
	}
	fmt.Println("PASS: shadow and opt-in automatic modes preserve capacity with missing metrics; API policy edits/defaults validated")
	for _, fleetName := range []string{"alpha", "beta"} {
		kind := h.workloadKind(fleetName)
		before := h.get(kind, fleetName)
		h.merge(fleetName, `{"spec":{"maintenance":{"paused":true}}}`)
		h.wait("pause preserves serving readiness: "+fleetName, func() bool {
			return annotation(h.get(kind, fleetName), "celld.example.com/maintenance-fence") == "paused" &&
				num(h.get("celldfleet", fleetName), "status", "readyReplicas") == 3 && h.ready(fleetName)
		})
		assert(same(sub(h.get(kind, fleetName), "spec"), sub(before, "spec")), "pause changed the workload")
		h.merge(fleetName, `{"spec":{"maintenance":{"paused":false,"restartToken":"integration-restart"}}}`)
		h.wait("restart request blocked: "+fleetName, func() bool {
			return hasReason(h.get("celldfleet", fleetName), "DisruptionUnqualified")
		})
		assert(same(sub(h.get(kind, fleetName), "spec"), sub(before, "spec")), "blocked restart changed the workload")
		h.merge(fleetName, `{"spec":{"runtimeImage":"ghcr.io/denoland/celld@sha256:`+strings.Repeat("a", 64)+`"}}`)
		h.wait("unqualified image transition blocked: "+fleetName, func() bool {
			return hasReason(h.get("celldfleet", fleetName), "UnsupportedTransition")
		})
		assert(same(sub(h.get(kind, fleetName), "spec"), sub(before, "spec")), "blocked transition changed the workload")
		h.merge(fleetName, `{"spec":{"runtimeImage":null,"maintenance":null}}`)
	}
	sts := h.get("statefulset", "beta")
	assert(same(sub(sts, "spec", "persistentVolumeClaimRetentionPolicy"), object{"whenDeleted": "Retain", "whenScaled": "Retain"}), "claim retention policy: %v", sub(sts, "spec", "persistentVolumeClaimRetentionPolicy"))
	assert(str(sts, "spec", "updateStrategy", "type") == "OnDelete", "update strategy: %v", sub(sts, "spec", "updateStrategy"))
	h.k("-n", "fleets", "delete", "celldfleet", "beta", "--wait=false")
	h.wait("deletion blocked without touching StatefulSet or PVCs", func() bool {
		return hasReason(h.get("celldfleet", "beta"), "DeletionBlocked")
	})
	assert(uidOf(h.get("statefulset", "beta")) == uidOf(sts), "blocked deletion replaced the StatefulSet")
	assert(len(h.listIn("fleets", "pvc", "-l", "celld.example.com/fleet-uid="+uidOf(h.get("celldfleet", "beta")))) == 3, "blocked deletion touched claims")
	// Controller restart must preserve reservation/journal and workload UIDs.
	deploymentUID := uidOf(h.get("deployment", "alpha"))
	h.process.stop(false)
	h.k("-n", "fleets", "patch", "celldfleet", "alpha", "--subresource=status", "--type=merge", "-p", `{"status":{"conditions":[],"readyReplicas":0}}`)
	h.startNativeOperator()
	h.waitFor("restarted controller reconciles readiness from durable reservation", 90*time.Second, func() bool { return h.ready("alpha") })
	assert(uidOf(h.get("deployment", "alpha")) == deploymentUID, "restart recreated the Deployment")
	assert(h.process.running(), "native operator exited")
	// Reservation baseline survives a replica edit before any workload exists.
	h.apply(&networkingv1.NetworkPolicy{
		APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy",
		Name: "delayed", Namespace: "fleets",
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"integration.example.com/unused": "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	})
	delayed := bucketFleet("delayed", "bucket-delayed")
	delayed.Spec.Replicas = 1
	h.apply(delayed)
	h.wait("reservation persists before blocked initial provisioning", func() bool {
		return hasReason(h.get("celldfleet", "delayed"), "InfrastructureBlocked")
	})
	h.setReplicas("delayed", 2)
	// This policy was created by this invocation and selects no runtime Pods.
	h.k("-n", "fleets", "delete", "networkpolicy", "delayed")
	h.wait("replica edit before workload creation preserves reservation baseline", func() bool {
		reservation := h.reservation("delayed")
		journalText := annotation(reservation, journalAnnotation)
		if journalText == "" {
			return false
		}
		journal := decode(journalText)
		return num(reservation, "spec", "initialReplicas") == 1 && num(journal, "Initial") == 1 && num(journal, "Applied") == 2
	})
	assert(specReplicas(h.get("deployment", "delayed")) == 2, "delayed fleet did not reach two replicas")
	fmt.Println("PASS: controller restart preserves initial workload; all integration assertions passed")
}

// reservation finds the storage reservation for a fleet in the fleets namespace.
func (h *harness) reservation(fleetName string) object {
	for _, value := range h.reservations() {
		if str(value, "spec", "fleetName") == fleetName && str(value, "spec", "fleetNamespace") == "fleets" {
			return value
		}
	}
	fail("no reservation for fleet %s", fleetName)
	return nil
}

// journal decodes the lifecycle journal annotation of a fleet's reservation.
func (h *harness) journal(fleetName string) object {
	text := annotation(h.reservation(fleetName), journalAnnotation)
	if text == "" {
		return nil
	}
	return decode(text)
}
