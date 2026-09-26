package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	admissionNS      = "celld-admission"
	admissionService = "admission"
	admissionLocal   = "celld-integration/admission:local"
	// admissionCanary is an extra Pod name the webhook admits, used for a
	// server-side dry run proving the webhook is active before fleets exist.
	admissionCanary = "admission-canary"
)

// admissionMembers are the Pod names the webhook mutates: every ordinal the
// admission fleets can reach, and nothing else in the cluster.
var admissionMembers = []string{"mesh-0", "mesh-1", "mesh-2", "meshb-0", "meshb-1", "meshb-2", admissionCanary}

// exerciseAdmission runs a PersistentFleet and an Ordered Bucket whose Pods
// are mutated on creation the way Istio sidecar injection, EKS IRSA and the
// Datadog admission controller mutate them (#57). Both fleets provision,
// upgrade their runtime, restart and scale 3 -> 2 -> 3 under continuous
// writes, and never report a blocked lifecycle.
func (h *harness) exerciseAdmission() {
	fmt.Println("EXTENDED: admission-mutated fleet Pods")
	image := h.buildAdmissionImage()
	h.deployAdmission(image)
	reasons := h.watchReasons("mesh", "meshb")

	source := h.extendedImage("bucket-mesh")
	upgrading := source != h.opts.runtimeImage
	mesh := h.smallFleet("mesh", "bucket-mesh", "PersistentFleet", 3)
	mesh.Spec.RuntimeImage = source
	meshb := h.smallFleet("meshb", "bucket-meshb", "Bucket", 3)
	meshb.Spec.BucketWorkload = "Ordered"
	meshb.Spec.RuntimeImage = source
	h.apply(mesh)
	h.apply(meshb)
	if upgrading {
		// A runtime without node-log state serves but never reports settled.
		h.waitFor("mesh serves on the upgrade source", 10*time.Minute, func() bool {
			ready := 0
			for _, pod := range h.memberPods("mesh") {
				if podReady(pod) {
					ready++
				}
			}
			return h.ready("mesh") && ready == 3
		})
	} else {
		h.waitSettled("mesh", 3, 10*time.Minute)
	}
	h.waitSettled("meshb", 3, 10*time.Minute)
	h.assertInjected("mesh")
	h.assertInjected("meshb")
	h.writeLedger("mesh")
	h.writeLedger("meshb")
	h.startWriter("mesh")
	h.startWriter("meshb")

	if upgrading {
		upgrade := encode(object{"spec": object{"runtimeImage": h.opts.runtimeImage}})
		onRuntime := func(fleetName string) func() bool {
			return func() bool {
				for _, pod := range h.memberPods(fleetName) {
					if celldImage(pod) != h.opts.runtimeImage {
						return false
					}
				}
				return true
			}
		}
		h.rollAll("mesh", upgrade, onRuntime("mesh"), nil)
		h.rollBucket("meshb", upgrade, onRuntime("meshb"))
		fmt.Println("PASS: runtime upgrade", source, "->", h.opts.runtimeImage, "with injected Pods")
		h.checkpoint("mesh")
		h.checkpoint("meshb")
	} else {
		fmt.Println("NOT RUN: runtime upgrade of admission-mutated fleets; --upgrade-from none")
	}

	restart := encode(object{"spec": object{"maintenance": object{"restartToken": "extended-admission"}}})
	h.rollAll("mesh", restart, nil, nil)
	h.rollBucket("meshb", restart, nil)
	h.checkpoint("mesh")
	h.checkpoint("meshb")

	h.shrinkWatchingRelease("mesh", 2, nil, nil)
	h.scale("mesh", 3)
	h.scale("meshb", 2)
	h.scale("meshb", 3)
	h.assertInjected("mesh")
	h.assertInjected("meshb")
	h.stopWriter("mesh")
	h.stopWriter("meshb")
	h.readLedger("mesh")
	h.readLedger("meshb")

	seen := reasons.stop()
	for fleetName, counts := range seen {
		for reason := range counts {
			assert(slices.Contains([]string{"Provisioned", "Provisioning", "LifecycleProgress"}, reason), "%s reported %s with injected Pods: %v", fleetName, reason, seen)
		}
	}
	fmt.Println("PASS: admission fleets reported only progress and Provisioned:", seen)
	h.deleteFleet("mesh")
	h.deleteFleet("meshb")
	fmt.Println("PASS: Istio-, IRSA- and Datadog-shaped admission does not block provisioning, upgrade, restart or scaling")
}

func celldImage(pod object) string {
	for _, c := range list(pod, "spec", "containers") {
		if str(c, "name") == "celld" {
			return str(c, "image")
		}
	}
	return ""
}

// rollBucket applies patch to an Ordered Bucket and waits until every member
// has a new Pod and the fleet has settled.
func (h *harness) rollBucket(fleetName, patch string, done func() bool) {
	count := specReplicas(h.get("statefulset", fleetName))
	before := nameUIDs(h.memberPods(fleetName))
	h.merge(fleetName, patch)
	h.waitFor("rolling "+fleetName, 15*time.Minute, func() bool {
		after := nameUIDs(h.memberPods(fleetName))
		for name, uid := range before {
			if after[name] == uid {
				return false
			}
		}
		return h.settled(fleetName, count) && (done == nil || done())
	})
	assert(h.budget(fleetName) == 1, "%s PDB does not allow one disruption after rolling", fleetName)
}

// buildAdmissionImage builds the webhook from source with the module's
// toolchain onto the operator's pinned distroless base, loads it into every
// node and returns it by manifest digest. Nothing is pushed or pulled.
func (h *harness) buildAdmissionImage() string {
	dir := h.path("admission-image")
	must(os.MkdirAll(dir, 0o755))
	h.goBuild(filepath.Join(dir, "admission"), "./hack/integration/admission")
	dockerfile, err := os.ReadFile(filepath.Join(h.root, "hack", "integration", "admission", "Dockerfile"))
	must(err)
	must(os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o600))
	h.sh(10*time.Minute, "docker", "build", "--platform", "linux/"+h.arch, "-t", admissionLocal, dir)
	archive := h.path("admission.tar")
	h.sh(3*time.Minute, "docker", "image", "save", "--platform", "linux/"+h.arch, "-o", archive, admissionLocal)
	h.sh(3*time.Minute, "kind", "load", "image-archive", "--name", h.name, archive)
	must(os.Remove(archive))
	listing := h.run(command{args: []string{"docker", "exec", h.nodes[0], "ctr", "-n", "k8s.io", "images", "ls"}, timeout: time.Minute})
	var loaded, digest string
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 2 && (fields[0] == admissionLocal || fields[0] == "docker.io/"+admissionLocal) {
			loaded, digest = fields[0], fields[2]
		}
	}
	assert(strings.HasPrefix(digest, "sha256:") && len(digest) == 71, "admission image manifest not found: %s", listing)
	image := "localhost/celld-admission@" + digest
	for _, node := range h.nodes {
		h.sh(time.Minute, "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "tag", "--force", loaded, image)
	}
	fmt.Println("Admission webhook image:", image)
	return image
}

// deployAdmission runs the webhook behind a TLS Service signed by a CA the
// harness generates, and registers it for Pod creation of the admission
// fleets' members only, failing closed.
func (h *harness) deployAdmission(image string) {
	caPEM, certPEM, keyPEM := admissionCertificates(admissionService + "." + admissionNS + ".svc")
	h.k("create", "namespace", admissionNS)
	h.apply(object{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata":   object{"name": "admission-tls", "namespace": admissionNS},
		"stringData": object{"tls.crt": string(certPEM), "tls.key": string(keyPEM)},
	})
	labels := object{"app": "admission"}
	h.apply(object{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": object{"name": "admission", "namespace": admissionNS},
		"spec": object{
			"replicas": 1, "selector": object{"matchLabels": labels},
			"template": object{
				"metadata": object{"labels": labels},
				"spec": object{
					"securityContext": object{"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": object{"type": "RuntimeDefault"}},
					"containers": []object{{
						"name": "admission", "image": image, "imagePullPolicy": "IfNotPresent",
						"args":            []string{"serve", "--image", image},
						"ports":           []object{{"containerPort": 8443}},
						"readinessProbe":  object{"httpGet": object{"path": "/healthz", "port": 8443, "scheme": "HTTPS"}, "periodSeconds": 2},
						"securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}},
						"volumeMounts":    []object{{"name": "tls", "mountPath": "/tls", "readOnly": true}},
					}},
					"volumes": []object{{"name": "tls", "secret": object{"secretName": "admission-tls"}}},
				},
			},
		},
	})
	h.apply(object{
		"apiVersion": "v1", "kind": "Service",
		"metadata": object{"name": admissionService, "namespace": admissionNS},
		"spec":     object{"selector": labels, "ports": []object{{"port": 443, "targetPort": 8443}}},
	})
	h.k("-n", admissionNS, "rollout", "status", "deployment/admission", "--timeout=180s")
	h.apply(object{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingWebhookConfiguration",
		"metadata": object{"name": "celld-integration-admission"},
		"webhooks": []object{{
			"name":                    "pods.admission.celld.eric.dev",
			"admissionReviewVersions": []string{"v1"},
			"sideEffects":             "None",
			"failurePolicy":           "Fail",
			"reinvocationPolicy":      "Never",
			"timeoutSeconds":          10,
			"clientConfig": object{
				"service":  object{"namespace": admissionNS, "name": admissionService, "path": "/mutate", "port": 443},
				"caBundle": caPEM,
			},
			"rules": []object{{
				"operations": []string{"CREATE"}, "apiGroups": []string{""}, "apiVersions": []string{"v1"},
				"resources": []string{"pods"}, "scope": "Namespaced",
			}},
			"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": "fleets"}},
			"objectSelector": object{"matchExpressions": []object{{
				"key": "statefulset.kubernetes.io/pod-name", "operator": "In", "values": admissionMembers,
			}}},
		}},
	})
	canary := sleepPod(admissionCanary, "fleets", map[string]string{"statefulset.kubernetes.io/pod-name": admissionCanary})
	h.waitFor("admission webhook mutates a matching Pod (server-side dry run)", 2*time.Minute, func() bool {
		out, err := h.try(command{args: h.kubectl("create", "--dry-run=server", "-o", "json", "-f", "-"), timeout: 30 * time.Second, stdin: encode(canary)})
		return err == nil && annotation(decode(out), "admission.celld.eric.dev/injected") == "true"
	})
}

// admissionCertificates returns a fresh CA and a serving certificate for host.
func admissionCertificates(host string) (caPEM, certPEM, keyPEM []byte) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "celld integration admission CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	must(err)
	caCert, err := x509.ParseCertificate(caDER)
	must(err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	must(err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	must(err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// assertInjected requires every member Pod to carry the shapes the webhook
// injects and its injected containers to be running and ready.
func (h *harness) assertInjected(fleetName string) {
	pods := h.memberPods(fleetName)
	assert(len(pods) > 0, "%s has no members", fleetName)
	for _, pod := range pods {
		must(injectedShapes(pod))
	}
	fmt.Printf("PASS: %d %s Pods carry the Istio, IRSA and Datadog shapes\n", len(pods), fleetName)
}

func injectedShapes(pod object) error {
	name := nameOf(pod)
	if annotation(pod, "admission.celld.eric.dev/injected") != "true" || str(pod, "metadata", "labels", "security.istio.io/tlsMode") != "istio" {
		return fmt.Errorf("%s was not mutated by the admission webhook", name)
	}
	inits, containers := list(pod, "spec", "initContainers"), list(pod, "spec", "containers")
	names := func(cs []object) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, str(c, "name"))
		}
		return out
	}
	if !slices.Contains(names(inits), "istio-init") || !slices.Contains(names(containers), "istio-proxy") || !slices.Contains(names(containers), "celld") {
		return fmt.Errorf("%s lacks injected containers: init %v, containers %v", name, names(inits), names(containers))
	}
	for _, c := range append(inits, containers...) {
		env := map[string]object{}
		for _, e := range list(c, "env") {
			env[str(e, "name")] = e
		}
		for _, key := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "DD_AGENT_HOST", "DD_ENTITY_ID", "DD_ENV"} {
			if _, ok := env[key]; !ok {
				return fmt.Errorf("%s container %s lacks %s", name, str(c, "name"), key)
			}
		}
		if str(env["DD_AGENT_HOST"], "valueFrom", "fieldRef", "fieldPath") != "status.hostIP" {
			return fmt.Errorf("%s container %s DD_AGENT_HOST is not the host IP", name, str(c, "name"))
		}
		mounted := slices.ContainsFunc(list(c, "volumeMounts"), func(m object) bool {
			return str(m, "name") == "aws-iam-token" && str(m, "mountPath") == "/var/run/secrets/eks.amazonaws.com/serviceaccount"
		})
		if !mounted {
			return fmt.Errorf("%s container %s lacks the IRSA token mount", name, str(c, "name"))
		}
	}
	projected := slices.ContainsFunc(list(pod, "spec", "volumes"), func(v object) bool {
		sources := list(v, "projected", "sources")
		return str(v, "name") == "aws-iam-token" && len(sources) == 1 && str(sources[0], "serviceAccountToken", "audience") == "sts.amazonaws.com"
	})
	if !projected {
		return fmt.Errorf("%s lacks the projected IRSA token volume", name)
	}
	if terminating(pod) {
		return nil
	}
	for _, status := range list(pod, "status", "containerStatuses") {
		if str(status, "name") == "istio-proxy" && field(status, "ready") != true {
			return fmt.Errorf("%s istio-proxy is not ready", name)
		}
	}
	return nil
}

// reasonWatch samples fleets' Ready reasons in the background. It never
// fails the run itself; the caller inspects what it saw.
type reasonWatch struct {
	mu   sync.Mutex
	seen map[string]map[string]int
	quit chan struct{}
	done chan struct{}
}

func (h *harness) watchReasons(fleets ...string) *reasonWatch {
	w := &reasonWatch{seen: map[string]map[string]int{}, quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			for _, fleetName := range fleets {
				out, err := h.tryK("-n", "fleets", "get", "celldfleet", fleetName, "--ignore-not-found", "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].reason}`)
				if reason := strings.TrimSpace(out); err == nil && reason != "" {
					w.mu.Lock()
					if w.seen[fleetName] == nil {
						w.seen[fleetName] = map[string]int{}
					}
					w.seen[fleetName][reason]++
					w.mu.Unlock()
				}
			}
			select {
			case <-w.quit:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return w
}

func (w *reasonWatch) stop() map[string]map[string]int {
	close(w.quit)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}
