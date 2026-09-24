package main

import (
	"fmt"
	"strings"
	"time"
)

func (h *harness) previewParent(name, bucket, previewBucket string, source object) {
	parent := h.bucketFleet(name, bucket)
	parent.Spec.Replicas = 1
	parent.Spec.Placement.Mode = "Relaxed"
	// These are ordinary existing parents. The fixture source is itself a preview
	// so both source and destination use the real custom-endpoint configuration.
	p := object{"runtimeImage": h.opts.runtimeImage, "serviceAccountName": "runtime", "zone": "us-east-1a",
		"storage": object{"bucket": previewBucket, "region": "us-east-1", "endpoint": object{"url": "http://minio.celld-test-store.svc:9000", "credentialsSecretName": "preview-store", "egress": object{"namespace": storeNS, "podLabels": object{"app": "minio"}}}},
		"routing": object{"baseDomain": "previews.test", "scheme": "http", "ingress": object{"className": "preview"}, "source": object{"namespace": "fleets", "podLabels": object{"app": "preview-edge"}}},
	}
	if source != nil {
		p["seeding"] = object{"executor": "celld-snapshot-v1", "sources": []object{{"name": "source", "fleetRef": object{"name": nameOf(source), "namespace": "fleets", "uid": uidOf(source)}}}}
	}
	obj := decode(encode(parent))
	obj["spec"].(object)["previews"] = p
	h.apply(obj)
}

func (h *harness) previewCommand(pod string, args ...string) command {
	all := append([]string{"-n", "fleets", "exec", pod, "--", "celld", "preview"}, args...)
	all = append(all, "--context", "preview-test", "--namespace", "fleets")
	return command{args: h.kubectl(all...), timeout: 8 * time.Minute}
}
func (h *harness) deployPreview(name, parent, config string, extra ...string) object {
	args := []string{name, "--fleet", parent, "--config", "/app/" + config, "--timeout-seconds", "420", "--json"}
	args = append(args, extra...)
	fmt.Println("Deploying preview through CLI:", name)
	out := h.run(h.previewCommand("preview-developer", args...))
	assert(strings.Contains(out, "previews.test"), "CLI did not return preview URL: %s", out)
	p := h.get("celldpreview", name)
	assert(str(p, "status", "phase") == "Ready", "CLI returned before preview Ready: %s", encode(p))
	return p
}
func (h *harness) previewHTTP(p object, path, method string) object {
	if method == "POST" {
		path += fmt.Sprintf("?alarm=%d", h.previewAlarm)
	}
	out := h.k("-n", "fleets", "exec", "preview-client", "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "10", "-X", method, str(p, "status", "url")+path)
	return decode(out)
}
func (h *harness) previewSeed(name string, selection []object) object {
	var p object
	// Build developer YAML with the real CLI, then submit it using the developer
	// identity. No status or controller-owned resources are fabricated.
	args := []string{name, "--fleet", "target-parent", "--ttl-seconds", "1800", "--seed-from", "source", "--dry-run"}
	for _, obj := range selection {
		args = append(args, "--object", str(obj, "class")+":"+str(obj, "id"))
	}
	yaml := h.run(h.previewCommand("preview-developer", args...))
	h.run(command{args: h.kubectl("-n", "fleets", "exec", "-i", "preview-developer", "--", "kubectl", "--context", "preview-test", "-n", "fleets", "create", "-f", "-"), stdin: yaml, timeout: time.Minute})
	h.waitFor("seed reservation for "+name, 90*time.Second, func() bool {
		p = h.get("celldpreview", name)
		res := str(p, "status", "seedReservation")
		if res == "" {
			return false
		}
		r, err := h.tryGet("", "celldstoragereservation", res)
		return err == nil && str(r, "spec", "initialization", "target", "previewUID") == uidOf(p)
	})
	h.assertPreviewStopped(p)
	return p
}
func (h *harness) assertPreviewStopped(p object) {
	child := h.get("celldfleet", str(p, "status", "fleetName"))
	assert(len(h.fleetPods(nameOf(child))) == 0, "seeded preview started before initialization")
	assert(len(h.listIn("ingresses", "-l", "celld.eric.dev/fleet-uid="+uidOf(child))) == 0, "seeded preview exposed routing before initialization")
	r := h.cluster("celldstoragereservation", str(p, "status", "seedReservation"))
	assert(str(r, "metadata", "annotations", "celld.eric.dev/seed-gate") == "", "startup gate opened before seed completion")
}
func (h *harness) deletePreview(p object) {
	h.run(h.previewCommand("preview-developer", "delete", nameOf(p)))
	h.waitFor("preview and child safely removed", 3*time.Minute, func() bool {
		out := h.k("-n", "fleets", "get", "celldpreview", nameOf(p), "--ignore-not-found", "-o", "name")
		child := h.k("-n", "fleets", "get", "celldfleet", str(p, "status", "fleetName"), "--ignore-not-found", "-o", "name")
		return strings.TrimSpace(out) == "" && strings.TrimSpace(child) == ""
	})
	h.assertPreviewRetained(p)
}
func (h *harness) assertPreviewRetained(p object) {
	child := str(p, "status", "fleetName")
	var reservation object
	for _, r := range h.reservations() {
		if str(r, "spec", "prefix") == child {
			reservation = r
		}
	}
	assert(reservation != nil, "preview prefix reservation was lost on deletion")
	selector := "celld.eric.dev/fleet-uid=" + str(reservation, "spec", "fleetUID")
	h.waitFor("preview routing and pods removed", time.Minute, func() bool {
		return len(h.listIn("ingresses", "-l", selector)) == 0 && len(h.listIn("pods", "-l", selector)) == 0
	})
	if str(p, "status", "phase") == "Ready" {
		out := h.k("-n", "fleets", "exec", "preview-store-client", "--", "mc", "ls", "--recursive", "local/"+str(reservation, "spec", "bucket")+"/"+child+"/")
		assert(strings.TrimSpace(out) != "", "preview data disappeared on shutdown")
	}
}
func (h *harness) checkPreviewState(p object, alarms bool) {
	for i, name := range []string{"one", "two"} {
		expected := []int{1, 2}[i]
		state := h.previewHTTP(p, "/counter/"+name, "GET")
		assert(num(state, "count") == int64(expected) && num(state, "rows") == int64(expected), "%s/%s KV/SQL mismatch: %s", nameOf(p), name, encode(state))
		if alarms {
			assert(num(state, "alarm") == h.previewAlarm, "source alarm not preserved: %s", encode(state))
		} else {
			assert(state["alarm"] == nil, "alarm was not cleared: %s", encode(state))
		}
	}
}

func (h *harness) exercisePreviews() {
	h.previewAlarm = time.Now().Add(24 * time.Hour).UnixMilli()
	h.deployPreviewEdge()
	h.deployPreviewTools()
	// RBAC assertions are evaluated by the API server using actual identities.
	for _, verb := range []string{"create", "patch", "delete"} {
		out, err := h.try(command{args: h.kubectl("-n", "fleets", "exec", "preview-developer", "--", "kubectl", "--context", "preview-test", "auth", "can-i", verb, "celldfleets", "-n", "fleets"), timeout: time.Minute})
		assert(err != nil && strings.HasPrefix(strings.TrimSpace(out), "no"), "developer can %s fleets: %s", verb, out)
	}
	h.previewParent("source-parent", "bucket-alpha", "preview-source", nil)
	source := h.deployPreview("source", "source-parent", "wrangler.jsonc")
	h.curl(request{pod: "preview-client", address: str(source, "status", "fleetName"), port: 8080, path: "/", denied: true})
	h.previewHTTP(source, "/counter/one", "POST")
	h.previewHTTP(source, "/counter/two", "POST")
	h.previewHTTP(source, "/counter/two", "POST")
	h.checkPreviewState(source, true)
	selection := []object{}
	for _, name := range []string{"one", "two"} {
		selection = append(selection, object{"class": "Counter", "id": str(h.previewHTTP(source, "/id/"+name, "GET"), "id")})
	}
	sourceChild := h.get("celldfleet", str(source, "status", "fleetName"))
	h.previewParent("target-parent", "bucket-beta", "preview-target", sourceChild)
	// Persisted LTX is the documented snapshot boundary. Wait for both objects
	// to checkpoint rather than assuming HTTP acknowledgement is an LTX upload.
	h.waitFor("both source objects checkpointed", 3*time.Minute, func() bool {
		for _, obj := range selection {
			path := "local/preview-source/" + str(source, "status", "fleetName") + "/cells/Counter:" + str(obj, "id") + "/ltx/"
			out, err := h.try(command{args: h.kubectl("-n", "fleets", "exec", "preview-store-client", "--", "mc", "ls", "--recursive", path), timeout: 15 * time.Second})
			if err != nil || !strings.Contains(out, ".ltx") {
				return false
			}
		}
		return true
	})
	cleared := h.previewSeed("clone-clear", selection)
	h.hold(6*time.Second, "no runtime or ingress before seed execution", func() bool { h.assertPreviewStopped(cleared); return true })
	h.run(h.previewCommand("preview-executor", "seed", str(cleared, "status", "seedReservation")))
	cleared = h.deployPreview("clone-clear", "target-parent", "wrangler.jsonc")
	h.checkPreviewState(cleared, false)
	assert(str(cleared, "status", "url") != str(source, "status", "url"), "previews share URL")
	reservation := h.cluster("celldstoragereservation", str(cleared, "status", "seedReservation"))
	assert(str(reservation, "status", "phase") == "Succeeded" && len(list(reservation, "status", "manifest", "objects")) == 2, "seed did not pin both objects: %s", encode(reservation))
	// Redeploy retains identity and state while waiting for observed app version.
	h.previewHTTP(cleared, "/counter/one", "POST")
	redeployed := h.deployPreview("clone-clear", "target-parent", "wrangler-v2.jsonc", "--revision", "v2")
	assert(uidOf(redeployed) == uidOf(cleared) && str(redeployed, "status", "url") == str(cleared, "status", "url"), "redeploy changed identity")
	assert(str(h.previewHTTP(redeployed, "/", "GET"), "version") == "v2", "CLI returned before new code served")
	assert(num(h.previewHTTP(redeployed, "/counter/one", "GET"), "count") == 2, "redeploy reset state")
	h.checkPreviewState(source, true)
	// The developer's single call creates, seeds, publishes and waits while the
	// independent admin watch process discovers and executes the request.
	h.k("-n", "fleets", "exec", "preview-executor", "--", "/bin/sh", "-c", `celld preview seed --watch --context preview-test --namespace fleets >/tmp/watch.log 2>&1 & echo $! >/tmp/watch.pid`)
	seedArgs := []string{"--seed-from", "source", "--alarms", "Preserve", "--ttl-seconds", "1800"}
	for _, obj := range selection {
		seedArgs = append(seedArgs, "--object", str(obj, "class")+":"+str(obj, "id"))
	}
	preserve := h.deployPreview("clone-preserve", "target-parent", "wrangler.jsonc", seedArgs...)
	h.k("-n", "fleets", "exec", "preview-executor", "--", "/bin/sh", "-c", `kill "$(cat /tmp/watch.pid)"`)
	h.checkPreviewState(preserve, true)
	assert(str(preserve, "status", "url") != str(cleared, "status", "url"), "previews in shared bucket have same URL")
	fmt.Println("PASS: multi-object clone, alarms, stable distinct URLs, redeploy and source isolation")
	// Pending cancellation is acknowledged by the operator without an executor.
	canceled := h.previewSeed("cancel-pending", selection)
	h.deletePreview(canceled)
	r := h.cluster("celldstoragereservation", str(canceled, "status", "seedReservation"))
	assert(str(r, "status", "phase") == "Canceled", "pending seed was not canceled")
	_, err := h.try(h.previewCommand("preview-executor", "seed", nameOf(r)))
	assert(err != nil, "executor accepted canceled seed")
	// A real running preview expires through the normal safety finalizer.
	expiring := h.deployPreview("expires", "target-parent", "wrangler.jsonc", "--ttl-seconds", "120")
	h.waitFor("TTL removes preview compute", 4*time.Minute, func() bool { return str(h.get("celldpreview", "expires"), "status", "phase") == "Expired" })
	assert(strings.TrimSpace(h.k("-n", "fleets", "get", "celldfleet", str(expiring, "status", "fleetName"), "--ignore-not-found", "-o", "name")) == "", "expired preview child retained")
	h.assertPreviewRetained(expiring)
	h.deletePreview(cleared)
	h.deletePreview(preserve)
	h.exerciseSeedInterruptions(selection)
	h.deletePreview(source)
	fmt.Println("PASS: TTL, cancellation, safe deletion and reservation retention")
}

// Stall storage while leaving Kubernetes and DNS reachable. This exercises real
// executor claims without patching reservation status or runtime startup gates.
func (h *harness) blockSeedStorage() {
	policy := resourceObject("networking.k8s.io/v1", "NetworkPolicy", "pause-seed-storage")
	policy["spec"] = object{"podSelector": object{"matchLabels": object{"app": "preview-executor"}}, "policyTypes": []string{"Egress"}, "egress": []object{{"ports": []object{{"port": 443}, {"port": 6443}, {"port": 53, "protocol": "TCP"}, {"port": 53, "protocol": "UDP"}}}}}
	h.apply(policy)
	h.hold(3*time.Second, "seed storage policy installed", func() bool { return true })
}
func (h *harness) startSeedProcess(reservation string) {
	h.k("-n", "fleets", "exec", "preview-executor", "--", "/bin/sh", "-c", `celld preview seed "$1" --context preview-test --namespace fleets >/tmp/seed.log 2>&1 & echo $! >/tmp/seed.pid`, "seed", reservation)
	h.waitFor("executor claimed reservation", time.Minute, func() bool {
		return str(h.cluster("celldstoragereservation", reservation), "status", "phase") == "Running"
	})
}
func (h *harness) exerciseSeedInterruptions(selection []object) {
	p := h.previewSeed("cancel-running", selection)
	reservation := str(p, "status", "seedReservation")
	h.blockSeedStorage()
	h.startSeedProcess(reservation)
	h.run(h.previewCommand("preview-developer", "delete", nameOf(p)))
	h.waitFor("operator requests running seed cancellation", time.Minute, func() bool {
		return sub(h.cluster("celldstoragereservation", reservation), "status")["canceled"] == true
	})
	h.assertPreviewStopped(p)
	assert(str(h.cluster("celldstoragereservation", reservation), "status", "phase") == "Running", "operator acknowledged active executor itself")
	h.k("-n", "fleets", "delete", "networkpolicy", "pause-seed-storage")
	h.waitFor("executor acknowledges cancellation after storage call returns", 3*time.Minute, func() bool {
		return str(h.cluster("celldstoragereservation", reservation), "status", "phase") == "Canceled"
	})
	h.waitFor("canceled running preview safely removed", time.Minute, func() bool {
		return strings.TrimSpace(h.k("-n", "fleets", "get", "celldpreview", nameOf(p), "--ignore-not-found", "-o", "name")) == ""
	})
	h.assertPreviewRetained(p)

	crashed := h.previewSeed("executor-crash", selection)
	reservation = str(crashed, "status", "seedReservation")
	h.blockSeedStorage()
	h.startSeedProcess(reservation)
	claim := str(h.cluster("celldstoragereservation", reservation), "status", "executorID")
	// Destroy the actual executor process and restart with no process memory.
	h.k("-n", "fleets", "delete", "pod", "preview-executor", "--wait=true")
	h.k("-n", "fleets", "delete", "networkpolicy", "pause-seed-storage")
	h.apply(h.previewToolPod("preview-executor"))
	h.k("-n", "fleets", "wait", "--for=condition=Ready", "pod/preview-executor", "--timeout=90s")
	out, err := h.try(h.previewCommand("preview-executor", "seed", reservation))
	assert(err != nil && strings.Contains(out, "cannot be stolen"), "replacement executor accepted Running claim: %s", out)
	h.run(h.previewCommand("preview-developer", "delete", nameOf(crashed)))
	h.restartOperator()
	h.hold(8*time.Second, "crashed claim stays fenced across operator restart", func() bool {
		r := h.cluster("celldstoragereservation", reservation)
		h.assertPreviewStopped(crashed)
		return str(r, "status", "phase") == "Running" && str(r, "status", "executorID") == claim && str(h.get("celldpreview", nameOf(crashed)), "metadata", "deletionTimestamp") != ""
	})
	// Deliberately leave the ambiguous claim fenced until the disposable cluster
	// is destroyed. Never fabricate an executor acknowledgement to force cleanup.
	fmt.Println("PASS: running cancellation and crash/restart claim fencing")
}
