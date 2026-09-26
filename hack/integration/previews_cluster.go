package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const previewEdgeImage = "traefik:v3.6.1"

// Import the actual OCI manifest digest, not the image's config ID. No registry
// or mutable runtime reference is needed, and CRD validation stays enabled.
func (h *harness) loadPreviewRuntime() {
	archive := h.path("preview-runtime.tar")
	h.sh(3*time.Minute, "docker", "image", "save", "--platform", "linux/"+h.arch, "-o", archive, h.opts.runtimeLocalImage)
	h.sh(3*time.Minute, "kind", "load", "image-archive", "--name", h.name, archive)
	must(os.Remove(archive))
	listing := h.run(command{args: []string{"docker", "exec", h.nodes[0], "ctr", "-n", "k8s.io", "images", "ls"}, timeout: time.Minute})
	var digest string
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[0] == h.opts.runtimeLocalImage {
			digest = fields[2]
		}
	}
	assert(strings.HasPrefix(digest, "sha256:") && len(digest) == 71, "local image manifest not found: %s", listing)
	h.opts.runtimeImage = "localhost/celld-preview@" + digest
	for _, node := range h.nodes {
		h.sh(time.Minute, "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "tag", h.opts.runtimeLocalImage, h.opts.runtimeImage)
	}
	fmt.Println("Preview runtime:", h.opts.runtimeImage)
}

func resourceObject(api, kind, name string) object {
	return object{"apiVersion": api, "kind": kind, "metadata": object{"name": name, "namespace": "fleets"}}
}
func (h *harness) previewRBAC(name, kind string, rules []object) {
	sa := resourceObject("v1", "ServiceAccount", name)
	h.apply(sa)
	role := resourceObject("rbac.authorization.k8s.io/v1", kind, name)
	role["rules"] = rules
	h.apply(role)
	binding := resourceObject("rbac.authorization.k8s.io/v1", kind+"Binding", name)
	binding["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name}
	binding["subjects"] = []object{{"kind": "ServiceAccount", "name": name, "namespace": "fleets"}}
	h.apply(binding)
}

func (h *harness) deployPreviewEdge() {
	h.previewRBAC("preview-edge", "ClusterRole", []object{
		{"apiGroups": []string{""}, "resources": []string{"services", "endpoints", "secrets", "nodes"}, "verbs": []string{"get", "list", "watch"}},
		{"apiGroups": []string{"discovery.k8s.io"}, "resources": []string{"endpointslices"}, "verbs": []string{"get", "list", "watch"}},
		{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"ingresses", "ingressclasses"}, "verbs": []string{"get", "list", "watch"}},
		{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"ingresses/status"}, "verbs": []string{"update"}},
	})
	class := resourceObject("networking.k8s.io/v1", "IngressClass", "preview")
	class["spec"] = object{"controller": "traefik.io/ingress-controller"}
	h.apply(class)
	svc := resourceObject("v1", "Service", "preview-edge")
	svc["spec"] = object{"selector": object{"app": "preview-edge"}, "ports": []object{{"port": 80, "targetPort": 8000}}}
	h.apply(svc)
	ip := str(h.get("service", "preview-edge"), "spec", "clusterIP")
	pod := resourceObject("v1", "Pod", "preview-edge")
	pod["metadata"].(object)["labels"] = object{"app": "preview-edge"}
	pod["spec"] = object{"serviceAccountName": "preview-edge", "containers": []object{{"name": "edge", "image": previewEdgeImage, "args": []string{
		"--entrypoints.web.address=:8000", "--providers.kubernetesingress=true", "--providers.kubernetesingress.ingressclass=preview", "--providers.kubernetesingress.ingressendpoint.ip=" + ip,
	}}}}
	h.apply(pod)
	h.k("-n", "fleets", "wait", "--for=condition=Ready", "pod/preview-edge", "--timeout=120s")
	// Only this disposable cluster's DNS is changed. Clients resolve each real
	// preview hostname and preserve it in the HTTP Host header through Traefik.
	cm := h.getIn("kube-system", "configmap", "coredns")
	corefile := str(cm, "data", "Corefile")
	corefile = strings.Replace(corefile, ".:53 {", ".:53 {\n    rewrite stop name regex .*\\.previews\\.test\\. preview-edge.fleets.svc.cluster.local. answer auto", 1)
	h.k("-n", "kube-system", "patch", "configmap", "coredns", "--type=merge", "-p", encode(object{"data": object{"Corefile": corefile}}))
	h.k("-n", "kube-system", "rollout", "restart", "deployment/coredns")
	h.k("-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s")
	h.probe("preview-client", "fleets", nil)
}

func (h *harness) deployPreviewTools() {
	h.previewRBAC("preview-developer", "Role", []object{
		{"apiGroups": []string{"celld.eric.dev"}, "resources": []string{"celldpreviews"}, "verbs": []string{"get", "create", "update", "patch", "delete"}},
		{"apiGroups": []string{"celld.eric.dev"}, "resources": []string{"celldfleets"}, "verbs": []string{"get"}},
	})
	h.previewRBAC("preview-executor", "ClusterRole", []object{
		{"apiGroups": []string{"celld.eric.dev"}, "resources": []string{"celldstoragereservations"}, "verbs": []string{"get", "list", "watch"}},
		{"apiGroups": []string{"celld.eric.dev"}, "resources": []string{"celldstoragereservations/status"}, "verbs": []string{"get", "patch"}},
	})
	// Same-namespace source and target reads; no fleet or preview mutation.
	role := resourceObject("rbac.authorization.k8s.io/v1", "Role", "preview-executor")
	role["rules"] = []object{{"apiGroups": []string{"celld.eric.dev"}, "resources": []string{"celldfleets", "celldpreviews"}, "verbs": []string{"get"}}}
	h.apply(role)
	binding := resourceObject("rbac.authorization.k8s.io/v1", "RoleBinding", "preview-executor")
	binding["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "preview-executor"}
	binding["subjects"] = []object{{"kind": "ServiceAccount", "name": "preview-executor", "namespace": "fleets"}}
	h.apply(binding)
	cm := resourceObject("v1", "ConfigMap", "preview-kubeconfig")
	cm["data"] = object{"config": `apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: https://kubernetes.default.svc
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
users:
- name: runner
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
contexts:
- name: preview-test
  context:
    cluster: local
    user: runner
    namespace: fleets
current-context: preview-test
`}
	h.apply(cm)
	h.sh(time.Minute, "docker", "exec", h.nodes[0], "cp", "/usr/bin/kubectl", "/opt/celld-test-kubectl")
	h.sh(time.Minute, "docker", "cp", filepath.Join(h.root, "hack", "integration", "preview-app"), h.nodes[0]+":/opt/celld-preview-app")
	for _, name := range []string{"preview-developer", "preview-executor"} {
		h.apply(h.previewToolPod(name))
	}
	h.k("-n", "fleets", "wait", "--for=condition=Ready", "pod/preview-developer", "pod/preview-executor", "--timeout=120s")
	// Publish the fixture for the ordinary parent runtimes too.
	for _, bucket := range []string{"bucket-alpha", "bucket-beta"} {
		h.k("-n", "fleets", "exec", "preview-developer", "--", "env", "CELLD_BUCKET=s3://"+bucket, "S3_ENDPOINT=http://minio.celld-test-store.svc:9000", "AWS_REGION=us-east-1", "celld", "deploy", "/app/wrangler.jsonc")
	}
	secret := resourceObject("v1", "Secret", "preview-store")
	secret["stringData"] = object{"accessKeyId": "qualification", "secretAccessKey": "qualification-only"}
	h.apply(secret)
	h.k("-n", storeNS, "run", "preview-buckets", "--restart=Never", "--image="+mcImage, "--command", "--", "/bin/sh", "-c", `mc alias set local http://minio:9000 qualification qualification-only && mc mb local/preview-source local/preview-target`)
	h.wait("preview buckets created", func() bool { return h.succeeded("preview-buckets") })
	// A separate read-only probe observes durable checkpoints; it does not write
	// receipts or bypass any controller/executor ownership transition.
	pod := resourceObject("v1", "Pod", "preview-store-client")
	pod["spec"] = object{"containers": []object{{"name": "mc", "image": mcImage, "command": []string{"/bin/sh", "-c", `mc alias set local http://minio.celld-test-store.svc:9000 qualification qualification-only && sleep 7200`}}}}
	h.apply(pod)
	h.k("-n", "fleets", "wait", "--for=condition=Ready", "pod/preview-store-client", "--timeout=90s")
}

func (h *harness) previewToolPod(name string) object {
	pod := resourceObject("v1", "Pod", name)
	pod["metadata"].(object)["labels"] = object{"app": name}
	pod["spec"] = object{"nodeName": h.nodes[0], "serviceAccountName": name, "containers": []object{{
		"name": "cli", "image": h.opts.runtimeImage, "command": []string{"/bin/sh", "-c", "sleep 7200"},
		"env":          []object{{"name": "KUBECONFIG", "value": "/kube/config"}, {"name": "AWS_ACCESS_KEY_ID", "value": "qualification"}, {"name": "AWS_SECRET_ACCESS_KEY", "value": "qualification-only"}, {"name": "AWS_ALLOW_HTTP", "value": "true"}},
		"volumeMounts": []object{{"name": "kubectl", "mountPath": "/usr/local/bin/kubectl", "readOnly": true}, {"name": "config", "mountPath": "/kube", "readOnly": true}, {"name": "app", "mountPath": "/app", "readOnly": true}},
	}}, "volumes": []object{{"name": "kubectl", "hostPath": object{"path": "/opt/celld-test-kubectl", "type": "File"}}, {"name": "config", "configMap": object{"name": "preview-kubeconfig"}}, {"name": "app", "hostPath": object{"path": "/opt/celld-preview-app", "type": "Directory"}}}}
	return pod
}
