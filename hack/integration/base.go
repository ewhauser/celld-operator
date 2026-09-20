package main

import (
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const automaticCapacity = `{"mode":"Automatic","minReplicas":%d,"maxReplicas":3,"minSamples":2,"sampleIntervalSeconds":30,"scaleInStabilizationSeconds":60,"scaleInCooldownSeconds":60,"cpuLowMillicores":200,"cpuHighMillicores":500,"memoryLowMiB":700,"memoryHighMiB":1000}`

// workloadKind is the workload behind a fleet: Deployment for Bucket, StatefulSet otherwise.
func (h *harness) workloadKind(fleetName string) string {
	f := h.get("celldfleet", fleetName)
	if str(f, "spec", "profile") == "Bucket" && str(f, "spec", "bucketWorkload") != "Ordered" {
		return "deployment"
	}
	return "statefulset"
}

// ready reports a fleet whose Ready condition is true and whose workload has
// observed its latest generation with every replica ready.
func (h *harness) ready(fleetName string) bool {
	current := h.get("celldfleet", fleetName)
	observed, err := h.tryGet("fleets", h.workloadKind(fleetName), fleetName)
	if err != nil {
		return false
	}
	observedGeneration := num(observed, "status", "observedGeneration")
	generation := generation(observed)
	if generation == 0 {
		generation = 1
	}
	return isReady(current) && observedGeneration >= generation && num(observed, "status", "readyReplicas") == specReplicas(observed)
}

func (h *harness) exerciseIsolation() {
	alpha := h.bucketFleet("alpha", "bucket-alpha")
	beta := h.newFleet("beta", "bucket-beta", "PersistentFleet", "fleets")
	// An old deterministic claim must never be adopted into a new bucket.
	h.apply(&corev1.PersistentVolumeClaim{
		APIVersion: "v1", Kind: "PersistentVolumeClaim",
		Name: "data-collision-0", Namespace: "fleets", Labels: map[string]string{"celld.eric.dev/fleet-uid": "previous-fleet"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: new("disposable"),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	})
	oldClaimUID := uidOf(h.get("pvc", "data-collision-0"))
	h.apply(h.newFleet("collision", "new-bucket", "PersistentFleet", "fleets"))
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
	pods := h.listIn("pods")
	addresses := map[string]string{}
	for _, fleetName := range []string{"alpha", "beta"} {
		uid := uidOf(h.get("celldfleet", fleetName))
		var selected []object
		for _, pod := range pods {
			if str(pod, "metadata", "labels", "celld.eric.dev/fleet-uid") == uid {
				selected = append(selected, pod)
			}
		}
		assert(len(selected) == 2, "%s has %d pods", fleetName, len(selected))
		assert(len(nodeNames(selected)) == 2, "%s replicas share a node", fleetName)
		addresses[fleetName] = str(selected[0], "status", "podIP")
	}
	fmt.Println("PASS: strict replicas use distinct nodes within the configured zone")
	unavailable := h.bucketFleet("unavailable", "bucket-unavailable")
	unavailable.Spec.BucketWorkload = "Deployment"
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
	h.probe("same-fleet", "fleets", map[string]string{"celld.eric.dev/fleet-uid": alphaUID})
	h.probe("client", "fleets", map[string]string{"celld.eric.dev/client-of": "alpha"})
	h.probe("client-beta", "fleets", map[string]string{"celld.eric.dev/client-of": "beta"})
	h.probe("untrusted", "fleets", map[string]string{})
	h.probe("operator", operatorNS, map[string]string{"app.kubernetes.io/name": "celld-operator"})
	h.curl(request{pod: "same-fleet", address: addresses["alpha"]})
	h.curl(request{pod: "same-fleet", address: addresses["beta"], denied: true})
	h.curl(request{pod: "untrusted", address: addresses["alpha"], denied: true})
	h.curl(request{pod: "client", address: addresses["alpha"], denied: true})
	h.curl(request{pod: "operator", address: addresses["alpha"], ns: operatorNS})
	h.curl(request{pod: "client", address: "alpha", path: "/.well-known/celld/health", port: 8080})
	h.curl(request{pod: "client", address: "beta", path: "/.well-known/celld/health", port: 8080, denied: true})
	assert(stored(h.app("client", "alpha", "PUT", "/?cell=integration&id=ack"), "ack"), "write not acknowledged")
	assert(h.ackStored("client", "alpha"), "acknowledged write unreadable")
	assert(!h.ackStored("client-beta", "beta"), "write visible from another fleet's storage scope")
	fmt.Println("PASS: application writes readable only in their own fleet storage scope")
	fmt.Println("PASS: same-fleet/operator peer access, cross-fleet/untrusted/client peer denial, ClusterIP health routing")
	h.k("-n", "fleets", "delete", "pod", "same-fleet", "--wait=true")
	// This probe uses the operator's network label and therefore also matches
	// its Deployment selector. It must not shadow later manager log selection.
	h.k("-n", operatorNS, "delete", "pod", "operator", "--wait=true")
	h.apply(h.newFleet("conflict", "bucket-alpha", "Bucket", "other"))
	h.wait("cross-namespace storage conflict blocked", func() bool {
		return hasReason(h.getIn("other", "celldfleet", "conflict"), "StorageScopeConflict")
	})
	h.apply(h.newFleet("denied", "bucket-denied", "Bucket", "unbound"))
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

// currentState decodes the bounded current-operation annotation.
func (h *harness) currentState(fleetName string) object {
	text := annotation(h.reservation(fleetName), "celld.eric.dev/current-operation")
	if text == "" {
		return nil
	}
	return decode(text)
}
