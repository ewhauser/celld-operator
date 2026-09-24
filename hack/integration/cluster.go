package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ewhauser/celld-operator/api/v1alpha1"
)

const (
	calicoURL     = "https://raw.githubusercontent.com/projectcalico/calico/v3.29.3/manifests/calico.yaml"
	calicoSHA     = "9a575859428b822a224dedafc4238555b6b0f910f2abf12983f20f871860914e"
	minioImage    = "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z"
	curlImage     = "curlimages/curl:8.12.1"
	mcImage       = "quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z"
	toxiproxy     = "ghcr.io/shopify/toxiproxy:2.12.0"
	metricsServer = "registry.k8s.io/metrics-server/metrics-server:v0.8.0"
	metricsURL    = "https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.8.0/components.yaml"
	metricsSHA    = "ff64d1a13b9ac3b0635f0dd985815fb44c23eed4706c04e5db1daadf6bc0a83b"
	// Per-node hostpath CSI driver (kubernetes-csi/csi-driver-host-path v1.18.0,
	// kubernetes-distributed deployment) for ReadWriteOncePod claims on kind.
	hostpathCSI = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-host-path/v1.18.0/deploy/kubernetes-distributed/hostpath/"
	storeNS     = "celld-test-store"
	operatorNS  = "celld-system"
)

var runtimeImagePin = regexp.MustCompile(`^ghcr.io/ewhauser/celld@sha256:[a-f0-9]{64}$`)

var csiManifests = []struct{ url, sha string }{
	{"https://raw.githubusercontent.com/kubernetes-csi/external-provisioner/v6.3.0/deploy/kubernetes/rbac.yaml", "0ee8427b746a1d3b695705b74c2d7fb165121110b1a10c0b6e204918d93e814f"},
	{hostpathCSI + "csi-hostpath-driverinfo.yaml", "997418490e0a69887d5587e7b6dcec9f81c53b1eb2312b1dbf7d257683c196f6"},
	{hostpathCSI + "csi-hostpath-plugin.yaml", "ab5ca63465cdc81e2f1ece90a1b85f74e958c9cd565d15ae761cb4e578c763ff"},
}

var csiImages = []string{
	"registry.k8s.io/sig-storage/csi-provisioner:v6.3.0",
	"registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0",
	"registry.k8s.io/sig-storage/hostpathplugin:v1.17.1",
	"registry.k8s.io/sig-storage/livenessprobe:v2.19.0",
}

const kindConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
nodes:
- role: control-plane
  labels:
    topology.kubernetes.io/zone: us-east-1a
- role: worker
  labels:
    topology.kubernetes.io/zone: us-east-1a
- role: worker
  labels:
    topology.kubernetes.io/zone: us-east-1a
`

func (h *harness) path(name string) string { return filepath.Join(h.tmp, name) }

func (h *harness) writeFile(name string, body []byte) string {
	must(os.WriteFile(h.path(name), body, 0o600))
	return h.path(name)
}

func verified(body []byte, sha, what string) []byte {
	sum := sha256.Sum256(body)
	assert(hex.EncodeToString(sum[:]) == sha, "checksum mismatch for %s", what)
	return body
}

func (h *harness) createCluster() {
	config := kindConfig
	if h.opts.suite == "previews" {
		config = strings.Split(config, "- role: worker")[0]
	}
	configPath := h.writeFile("kind.yaml", []byte(config))
	fmt.Println("Creating isolated cluster", h.name)
	// Name is unique; cleanup is authorized only for this invocation's cluster.
	h.created = true
	h.sh(5*time.Minute, "kind", "create", "cluster", "--name", h.name, "--image", "kindest/node:v1.31.4", "--config", configPath, "--kubeconfig", h.kubeconfig)
	calico := h.writeFile("calico.yaml", verified(h.fetch(calicoURL, time.Minute), calicoSHA, calicoURL))
	h.k("apply", "-f", calico)
	h.k("wait", "--for=condition=Ready", "nodes", "--all", "--timeout=240s")
	if len(h.nodes) > 1 {
		h.k("taint", "nodes", h.nodes[0], "node-role.kubernetes.io/control-plane:NoSchedule-")
	}
	h.k("-n", "kube-system", "rollout", "status", "daemonset/calico-node", "--timeout=240s")
	h.k("apply", "-f", filepath.Join(h.root, "config", "crd"))
	h.k("wait", "--for=condition=Established", "crd/celldfleets.celld.eric.dev", "--timeout=60s")
	// Manager manifest is validated, but run the native binary under the exact SA RBAC below.
	h.k("apply", "-f", filepath.Join(h.root, "config", "manager", "operator.yaml"))
	h.k("-n", operatorNS, "scale", "deployment/celld-operator", "--replicas=0")
	for _, ns := range []string{"fleets", "other", storeNS, "unbound"} {
		h.k("create", "namespace", ns)
		h.k("-n", ns, "create", "serviceaccount", "runtime")
	}
	// Fleet-namespace privileges are granted per namespace; `unbound`
	// deliberately receives none.
	for _, ns := range []string{"fleets", "other"} {
		h.k("-n", ns, "apply", "-f", filepath.Join(h.root, "config", "rbac", "fleet-namespace.yaml"))
	}
	h.arch = str(h.cluster("node", h.nodes[0]), "status", "nodeInfo", "architecture")
}

func (h *harness) loadImages() {
	images := []string{h.opts.runtimeImage, minioImage, mcImage, curlImage, metricsServer, toxiproxy}
	if h.opts.upgradeImage != "" {
		images = append(images, h.opts.upgradeImage)
	}
	if h.opts.operatorImage != "" {
		images = append(images, h.opts.operatorImage)
	}
	if h.opts.suite == "previews" {
		images = []string{minioImage, mcImage, curlImage, previewEdgeImage}
		if h.opts.runtimeLocalImage != "" {
			h.loadPreviewRuntime()
		} else {
			images = append(images, h.opts.runtimeImage)
		}
		if h.opts.operatorImage != "" {
			images = append(images, h.opts.operatorImage)
		}
	} else {
		images = append(images, csiImages...)
	}
	for index, image := range images {
		// Docker's containerd store may have only the local platform of
		// a multiarch image. Export that platform explicitly; ordinary
		// kind load docker-image can fail on absent sibling manifests.
		_, notCached := h.try(command{args: []string{"docker", "image", "inspect", image}, timeout: 30 * time.Second})
		archive := h.path(fmt.Sprintf("cached-image-%d.tar", index))
		if notCached == nil && !strings.Contains(image, "@sha256:") {
			h.sh(3*time.Minute, "docker", "image", "save", "--platform", "linux/"+h.arch, "-o", archive, image)
			h.sh(3*time.Minute, "kind", "load", "image-archive", "--name", h.name, archive)
			must(os.Remove(archive))
			fmt.Println("Loaded cached image:", image)
			continue

		}
		h.pullImage(image)
	}
}

// Each node owns a separate containerd store. Bound concurrency at the three
// disposable nodes; a failure is collected before the parent performs cleanup.
func (h *harness) pullImage(image string) {
	fmt.Println("Pulling into disposable nodes:", image)
	results := make(chan error, len(h.nodes))
	for _, node := range h.nodes {
		go func() {
			var err error
			for attempt := range 3 {
				_, err = h.try(command{args: []string{"docker", "exec", node, "crictl", "pull", image}, timeout: 10 * time.Minute})
				if err == nil {
					fmt.Println("Pulled image:", image, "on", node)
					break
				}
				if h.ctx.Err() != nil {
					break
				}
				if attempt < 2 {
					fmt.Println("Retrying image pull on", node, "after:", truncate(err.Error(), 200))
				}
			}
			results <- err
		}()
	}
	var failures []error
	for range h.nodes {
		if err := <-results; err != nil {
			failures = append(failures, err)
		}
	}
	must(errors.Join(failures...))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sleepPod(name, ns string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		APIVersion: "v1", Kind: "Pod",
		Name: name, Namespace: ns, Labels: labels,
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: strings.SplitN(name, "-", 2)[0], Image: curlImage, Command: []string{"/bin/sh", "-c", "sleep 3600"},
		}}},
	}
}

func (h *harness) storeService(name, role string, ports ...int32) {
	svc := &corev1.Service{
		APIVersion: "v1", Kind: "Service",
		Name: name, Namespace: storeNS,
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "minio", "role": role}},
	}
	for _, port := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("p%d", port), Port: port, TargetPort: intstr.FromInt32(port)})
	}
	h.apply(svc)
}

func (h *harness) deployStore() {
	// Runtime egress admits store-namespace pods labeled app=minio. In
	// faults mode the fixed `minio` name resolves to toxiproxy, which
	// forwards to the real server under `minio-backend`.
	h.apply(&corev1.Pod{
		APIVersion: "v1", Kind: "Pod",
		Name: "minio", Namespace: storeNS, Labels: map[string]string{"app": "minio", "role": "backend"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "minio", Image: minioImage, Args: []string{"server", "/data"},
			Env:          []corev1.EnvVar{{Name: "MINIO_ROOT_USER", Value: "qualification"}, {Name: "MINIO_ROOT_PASSWORD", Value: "qualification-only"}},
			VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			Resources:    corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
		}}, Volumes: []corev1.Volume{{Name: "data", EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse("2Gi"))}}}},
	})
	h.k("-n", storeNS, "wait", "--for=condition=Ready", "pod/minio", "--timeout=120s")
	if h.opts.suite == "all" || h.opts.suite == "faults" {
		h.storeService("minio-backend", "backend", 9000)
		h.apply(&corev1.ConfigMap{
			APIVersion: "v1", Kind: "ConfigMap",
			Name: "toxiproxy", Namespace: storeNS,
			Data: map[string]string{"toxiproxy.json": encode([]object{{
				"name": "minio", "listen": "0.0.0.0:9000", "upstream": "minio-backend." + storeNS + ".svc:9000", "enabled": true,
			}})},
		})
		h.apply(&corev1.Pod{
			APIVersion: "v1", Kind: "Pod",
			Name: "toxiproxy", Namespace: storeNS, Labels: map[string]string{"app": "minio", "role": "proxy"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "toxiproxy", Image: toxiproxy, Args: []string{"-config", "/config/toxiproxy.json", "-host", "0.0.0.0"},
					VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/config", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{Name: "config", ConfigMap: &corev1.ConfigMapVolumeSource{Name: "toxiproxy"}}},
			},
		})
		h.storeService("minio", "proxy", 9000)
		h.storeService("toxiproxy-api", "proxy", 8474)
		ctl := sleepPod("toxi-ctl", storeNS, nil)
		ctl.Spec.Containers[0].Name = "ctl"
		ctl.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "sleep 7200"}
		h.apply(ctl)
		h.k("-n", storeNS, "wait", "--for=condition=Ready", "pod/toxiproxy", "pod/toxi-ctl", "--timeout=120s")
	} else {
		h.storeService("minio", "backend", 9000)
	}
	h.k("-n", storeNS, "run", "seed", "--restart=Never", "--image="+mcImage, "--command", "--", "/bin/sh", "-c",
		`attempt=0; until mc alias set local http://minio:9000 qualification qualification-only; do attempt=$((attempt+1)); test "$attempt" -lt 30 || exit 1; sleep 2; done; mc mb local/bucket-alpha local/bucket-beta`)
	h.wait("create two isolated test buckets", func() bool { return h.succeeded(storeNS, "seed") })
}

// deployApplication publishes the qualification app into both buckets with a
// checksum-verified esbuild binary staged on the control-plane node.
func (h *harness) deployApplication() {
	packages := map[string][2]string{
		"arm64": {"arm64", "8bwX7a8FghIgrupcxb4aUmYDLp8pX06rGh5HqDT7bB+8Rdells6mHvrFHHW2JAOPZUbnjUpKTLg6ECyzvas2AQ=="},
		"amd64": {"x64", "uqZMTLr/zR/ed4jIGnwSLkaHmPjOjJvnm6TVVitAa08SLS9Z0VM8wIRx7gWbJB5/J54YuIMInDquWyYvQLZkgw=="},
	}
	pkg, ok := packages[h.arch]
	assert(ok, "unsupported node architecture %q", h.arch)
	archive := h.fetch(fmt.Sprintf("https://registry.npmjs.org/@esbuild/linux-%s/-/linux-%s-0.25.12.tgz", pkg[0], pkg[0]), 2*time.Minute)
	sum := sha512.Sum512(archive)
	assert(base64.StdEncoding.EncodeToString(sum[:]) == pkg[1], "esbuild package integrity mismatch")
	esbuild := h.writeFile("esbuild", extractFromTarGz(archive, "package/bin/esbuild"))
	must(os.Chmod(esbuild, 0o755))
	h.sh(30*time.Second, "docker", "cp", esbuild, h.nodes[0]+":/opt/celld-test-esbuild")
	h.sh(30*time.Second, "docker", "cp", filepath.Join(h.root, "hack", "integration", "app"), h.nodes[0]+":/opt/celld-test-app")
	for _, bucket := range []string{"bucket-alpha", "bucket-beta"} {
		deployName := "deploy-" + bucket
		h.apply(&corev1.Pod{
			APIVersion: "v1", Kind: "Pod",
			Name: deployName, Namespace: storeNS,
			Spec: corev1.PodSpec{
				NodeName: h.nodes[0], RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name: "deploy", Image: h.opts.runtimeImage, Args: []string{"deploy", "/app"},
					Env: []corev1.EnvVar{
						{Name: "CELLD_BUCKET", Value: "s3://" + bucket}, {Name: "AWS_REGION", Value: "us-east-1"},
						{Name: "AWS_ALLOW_HTTP", Value: "true"}, {Name: "S3_ENDPOINT", Value: "http://minio:9000"},
						{Name: "AWS_ACCESS_KEY_ID", Value: "qualification"}, {Name: "AWS_SECRET_ACCESS_KEY", Value: "qualification-only"},
						{Name: "CELLD_ESBUILD", Value: "/esbuild"},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "app", MountPath: "/app", ReadOnly: true}, {Name: "esbuild", MountPath: "/esbuild", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{
					{Name: "app", HostPath: &corev1.HostPathVolumeSource{Path: "/opt/celld-test-app"}},
					{Name: "esbuild", HostPath: &corev1.HostPathVolumeSource{Path: "/opt/celld-test-esbuild"}},
				},
			},
		})
		h.waitFor("qualification app deployed to "+bucket, 120*time.Second, func() bool { return h.succeeded(storeNS, deployName) })
	}
}

func extractFromTarGz(archive []byte, member string) []byte {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	must(err)
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		must(err)
		if header.Name == member {
			body, err := io.ReadAll(reader)
			must(err)
			return body
		}
	}
}

func (h *harness) installStorageClass() {
	class := &storagev1.StorageClass{
		APIVersion: "storage.k8s.io/v1", Kind: "StorageClass",
		Name:              "disposable",
		Provisioner:       "hostpath.csi.k8s.io",
		ReclaimPolicy:     new(corev1.PersistentVolumeReclaimDelete),
		VolumeBindingMode: new(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	// SHA-pinned upstream manifests, applied as published; the driver runs
	// on every node so strict hostname separation still has three hosts.
	for _, manifest := range csiManifests {
		h.applyText(string(verified(h.fetch(manifest.url, time.Minute), manifest.sha, manifest.url)))
	}
	h.k("rollout", "status", "daemonset/csi-hostpathplugin", "--timeout=240s")
	class.Parameters = map[string]string{"kind": "fast"}
	h.apply(class)
}

func (h *harness) goBuild(output, pkg string) {
	h.run(command{
		args: []string{"go", "build", "-o", output, pkg}, dir: h.root, timeout: 5 * time.Minute,
		env: append(append([]string{}, h.env...), "GOOS=linux", "GOARCH="+h.arch, "CGO_ENABLED=0"),
	})
	must(os.Chmod(output, 0o755))
}

func (h *harness) startOperator() {
	args := []string{"--operator-namespace=" + operatorNS, "--network-policy-enforced", "--local-test", "--local-rwop"}
	container := object{"name": "operator"}
	var volumes []object
	if h.opts.operatorImage != "" {
		container["image"] = h.opts.operatorImage
		h.launcherImage = h.opts.operatorImage
	} else {
		h.goBuild(h.path("operator"), "./cmd/celld-operator")
		h.sh(30*time.Second, "docker", "cp", h.path("operator"), h.nodes[0]+":/opt/celld-test-operator")
		container["image"] = h.opts.runtimeImage
		container["command"] = []string{"/operator"}
		container["volumeMounts"] = []object{{"name": "operator-binary", "mountPath": "/operator", "readOnly": true}}
		volumes = []object{{"name": "operator-binary", "hostPath": object{"path": "/opt/celld-test-operator", "type": "File"}}}
		h.goBuild(h.path("celld-launcher"), "./cmd/celld-launcher")
		h.launcherImage = "celld-launcher-test:" + h.name
		h.builtLauncher = true
		base := h.opts.runtimeImage
		if h.opts.runtimeLocalImage != "" {
			base = h.opts.runtimeLocalImage
		}
		h.writeFile("Dockerfile", []byte("FROM "+base+"\nCOPY celld-launcher /celld-launcher\n"))
		h.sh(3*time.Minute, "docker", "build", "-t", h.launcherImage, h.tmp)
		h.sh(3*time.Minute, "kind", "load", "docker-image", "--name", h.name, h.launcherImage)
	}
	args = append(args, "--launcher-image="+h.launcherImage)
	container["args"] = args
	h.operatorArgs = args
	spec := object{"nodeName": h.nodes[0], "securityContext": object{"runAsUser": 65532}, "containers": []object{container}}
	if volumes != nil {
		spec["volumes"] = volumes
	}
	h.k("-n", operatorNS, "patch", "deployment", "celld-operator", "--type=strategic", "-p", encode(object{"spec": object{"replicas": 1, "template": object{"spec": spec}}}))
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=120s")
	if h.opts.suite == "previews" {
		return
	}
	metrics := h.writeFile("metrics.yaml", verified(h.fetch(metricsURL, 2*time.Minute), metricsSHA, metricsURL))
	h.k("apply", "-f", metrics)
	h.k("-n", "kube-system", "patch", "deployment", "metrics-server", "--type=json", "-p", `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`)
	h.k("-n", "kube-system", "rollout", "status", "deployment/metrics-server", "--timeout=300s")

}

// setOperatorFault rolls the in-cluster manager with an explicit --local-test
// crash point; an empty point withdraws it and replaces the crash-looping pod.
func (h *harness) setOperatorFault(point string) string {
	args := append([]string{}, h.operatorArgs...)
	if point != "" {
		args = append(args, "--local-fault-point="+point)
	}
	h.k("-n", operatorNS, "patch", "deployment", "celld-operator", "--type=json", "-p",
		encode([]object{{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": args}}))
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=180s")
	// Bind crash evidence to the new manager Pod, not kubectl's choice of a
	// matching Pod while a rollout or process restart changes readiness.
	var selected string
	h.wait("one manager Pod with the requested fault configuration", func() bool {
		selected = ""
		for _, pod := range items(decode(h.k("-n", operatorNS, "get", "pods", "-l", "app.kubernetes.io/name=celld-operator,pod-template-hash", "-o", "json"))) {
			if str(pod, "metadata", "deletionTimestamp") != "" {
				continue
			}
			for _, container := range list(pod, "spec", "containers") {
				if str(container, "name") == "operator" && same(strs(container, "args"), args) {
					if selected != "" {
						return false
					}
					selected = nameOf(pod)
				}
			}
		}
		return selected != ""
	})
	return selected
}

func (h *harness) restartOperator() {
	h.k("-n", operatorNS, "rollout", "restart", "deployment/celld-operator")
	h.k("-n", operatorNS, "rollout", "status", "deployment/celld-operator", "--timeout=120s")
}

func (h *harness) newFleet(name, bucket, profile, namespace string) *v1alpha1.CelldFleet {
	storage := v1alpha1.StorageSpec{Bucket: bucket, Region: "us-east-1", SizeGiB: 1}
	if profile == "PersistentFleet" {
		storage.StorageClassName = "disposable"
	}
	return &v1alpha1.CelldFleet{
		APIVersion: "celld.eric.dev/v1alpha1", Kind: "CelldFleet",
		Name: name, Namespace: namespace,
		Spec: v1alpha1.CelldFleetSpec{
			Profile: profile, Replicas: 2, RuntimeImage: h.opts.runtimeImage, ServiceAccountName: "runtime",
			Storage:   storage,
			Placement: v1alpha1.PlacementSpec{AZCount: 1, Zones: []string{"us-east-1a"}},
		},
	}
}

func (h *harness) bucketFleet(name, bucket string) *v1alpha1.CelldFleet {
	f := h.newFleet(name, bucket, "Bucket", "fleets")
	f.Spec.BucketWorkload = "Ordered"
	return f
}

// probe starts a curl pod. A fleet-uid label also sets an owner reference so the
// ReplicaSet cannot adopt it and it stays out of ready Service endpoints.
func (h *harness) probe(name, ns string, labels map[string]string) {
	pod := sleepPod(name, ns, labels)
	pod.Spec.Containers[0].Name = "probe"
	pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "integration.celld.eric.dev/NotServing"}}
	if uid, ok := labels["celld.eric.dev/fleet-uid"]; ok {
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "celld.eric.dev/v1alpha1", Kind: "CelldFleet", Name: "alpha", UID: types.UID(uid), Controller: new(true)}}
	}
	h.apply(pod)
	h.waitFor("probe "+name+" running", 90*time.Second, func() bool { return str(h.getIn(ns, "pod", name), "status", "phase") == "Running" })
}

type request struct {
	pod, ns, address, path, method string
	port                           int
	denied                         bool
}

// curl performs one HTTP request from inside a probe pod and asserts whether
// the network policy admitted it. Defaults target the private /state endpoint.
func (h *harness) curl(r request) string {
	if r.ns == "" {
		r.ns = "fleets"
	}
	if r.path == "" {
		r.path = "/state"
	}
	if r.port == 0 {
		r.port = 8081
	}
	if r.method == "" {
		r.method = "GET"
	}
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	args := h.kubectl("-n", r.ns, "exec", r.pod, "--", "curl", "--fail", "--silent", "--show-error", "--max-time", "3", "-X", r.method,
		fmt.Sprintf("http://%s:%d%s", r.address, r.port, r.path))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if h.ctx.Err() != nil {
		fail("integration interrupted")
	}
	allowed := err == nil
	assert(allowed == !r.denied, "%s -> %s allowed=%v: %s %s", r.pod, r.address, !r.denied, stdout.String(), stderr.String())
	if allowed && r.path == "/state" {
		_, hasLoad := decode(stdout.String())["node_load"]
		assert(hasLoad, "/state response lacks node_load: %s", stdout.String())
	}
	return stdout.String()
}

// app talks to the qualification application through the fleet's ClusterIP.
func (h *harness) app(pod, fleetName, method, path string) object {
	return decode(h.curl(request{pod: pod, address: fleetName, path: path, port: 8080, method: method}))
}

func stored(response object, expectedID string) bool {
	assert(expectedID != "" && str(response, "id") == expectedID, "response is not for requested ID %s: %v", expectedID, response)
	value, ok := field(response, "stored").(bool)
	assert(ok, "response lacks a boolean stored field: %v", response)
	return value
}

func (h *harness) ackStored(pod, fleetName string) bool {
	return stored(h.app(pod, fleetName, "GET", "/?cell=integration&id=ack"), "ack")
}
