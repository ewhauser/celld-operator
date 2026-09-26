package main

import (
	"fmt"
	"path/filepath"
	"time"
)

const (
	// previousRelease is the last release that ran PersistentFleet under the
	// strict launcher. Its manifests are vendored in testdata/.
	previousRelease = "v0.0.5"
	// previousOperatorImage is the signed v0.0.5 release image. It also holds
	// /celld-launcher, which v0.0.5 installs into PersistentFleet Pods.
	previousOperatorImage = "ghcr.io/ewhauser/celld-operator@sha256:0eedc0ae809661db68560ff34fccbc20e942cf1444d2e0313c1d42462566fb72"
	// previousRuntimeImage is the runtime v0.0.5 was qualified with.
	previousRuntimeImage = legacyRuntimeImage
)

// exerciseUpgrade reproduces #60 on the released v0.0.5 operator and requires
// this build to recover it with no manual step. Under v0.0.5, an ordinary Pod
// deletion makes the launcher permanently retire the member's disk, and the
// member never returns. After the operator is upgraded, the StatefulSet rolls
// every member onto plain celld on its own disk, the retired member rejoins
// on that disk, and every acknowledged write stays readable. No finalizer,
// reservation, claim or workload is edited by hand.
func (h *harness) exerciseUpgrade() {
	h.startPreviousOperator()
	beta := h.newFleet("beta", "bucket-beta", "PersistentFleet", "fleets")
	beta.Spec.RuntimeImage = previousRuntimeImage
	beta.Spec.Replicas = 3
	h.apply(beta)
	h.waitFor(previousRelease+" provisions a three-member PersistentFleet", 10*time.Minute, func() bool { return h.ready("beta") })
	h.writeLedger("beta")
	disks := h.claims("beta")
	assert(len(disks) == 3, "%s created %d claims, not 3", previousRelease, len(disks))

	h.k("-n", "fleets", "delete", "pod", "beta-2", "--wait=true")
	h.hold(90*time.Second, previousRelease+" leaves the member whose disk it retired down", func() bool {
		pod, err := h.tryGet("fleets", "pod", "beta-2")
		return err != nil || !podReady(pod)
	})

	d := h.watchDisruptions("beta")
	h.upgradeOperator()
	h.waitWatching("upgraded operator recovers beta on its own disks", 20*time.Minute, d, func() bool { return h.settled("beta", 3) })
	assert(len(d.replaced) == 3, "the upgrade rolled only %v onto the current template", d.replaced)
	must(sameDisks(disks, h.claims("beta")))
	h.assertDirectRuntime("beta")
	h.readLedger("beta")
	state := str(h.reservation("beta"), "metadata", "annotations", "celld.eric.dev/current-operation")
	assert(state == "", "upgraded operator kept %s bookkeeping on the reservation: %s", previousRelease, state)
	fmt.Println("PASS: upgrade from", previousRelease, "recovered a retired disk in place with every acknowledged write readable and nothing edited by hand")
}

// startPreviousOperator runs the released v0.0.5 manager, which installs its
// own image as the PersistentFleet launcher.
func (h *harness) startPreviousOperator() {
	args := []string{"--operator-namespace=" + operatorNS, "--network-policy-enforced", "--local-test", "--local-rwop", "--launcher-image=" + previousOperatorImage}
	container := object{"name": "operator", "image": previousOperatorImage, "args": args}
	spec := object{"nodeName": h.nodes[0], "securityContext": object{"runAsUser": 65532}, "containers": []object{container}}
	h.k("-n", operatorNS, "patch", "deployment", "celld-operator", "--type=strategic", "-p", encode(object{"spec": object{"replicas": 1, "template": object{"spec": spec}}}))
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=180s")
	fmt.Println("PASS: released", previousRelease, "operator running")
}

// upgradeOperator applies this build's CRDs, manager manifest and fleet Role
// over the release's, as an administrator upgrading the operator would, and
// starts this build's manager.
func (h *harness) upgradeOperator() {
	h.installManifests(filepath.Join(h.root, "config"))
	h.startOperator()
	fmt.Println("PASS: operator upgraded from", previousRelease)
}
