// Command admission is the extended integration suite's stand-in for the
// mutating admission webhooks a production cluster runs in front of fleet
// Pods: Istio sidecar injection, EKS IRSA (pod identity) and the Datadog
// admission controller. It reproduces their Pod shapes, not their behavior:
// the injected "proxy" intercepts no traffic, so the suite qualifies the
// operator's tolerance of injected containers, volumes, labels and
// environment (#57), not mesh routing.
//
// One static binary serves every role:
//
//	admission serve --cert F --key F --image I   TLS webhook on :8443
//	admission init                               the injected init container
//	admission sidecar                            the injected proxy container
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
)

type object = map[string]any

// Injected shapes. The suite asserts these exact names and values.
const (
	InjectedAnnotation = "admission.celld.eric.dev/injected"
	InitName           = "istio-init"
	ProxyName          = "istio-proxy"
	TokenVolume        = "aws-iam-token"
	TokenDir           = "/var/run/secrets/eks.amazonaws.com/serviceaccount"
	RoleARN            = "arn:aws:iam::111122223333:role/celld-integration"
	ProxyHealthPort    = 15021
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: admission serve|init|sidecar")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "init":
		// istio-init would program iptables here. This stand-in must not
		// intercept traffic, so it only proves it ran.
		fmt.Println("init: no traffic interception")
	case "sidecar":
		os.Exit(sidecar())
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", os.Args[1])
		os.Exit(2)
	}
}

// sidecar serves a readiness endpoint, as istio-proxy does, and exits on
// SIGTERM. It never touches the application's traffic.
func sidecar() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz/ready", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	server := &http.Server{Addr: fmt.Sprintf(":%d", ProxyHealthPort), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return 0
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cert := fs.String("cert", "/tls/tls.crt", "serving certificate")
	key := fs.String("key", "/tls/tls.key", "serving key")
	image := fs.String("image", "", "image for the injected containers (this binary's own image)")
	addr := fs.String("addr", ":8443", "listen address")
	if err := fs.Parse(args); err != nil || *image == "" {
		fmt.Fprintln(os.Stderr, "serve needs --image")
		return 2
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := review(body, *image)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Println("admission webhook listening on", *addr)
	if err := server.ListenAndServeTLS(*cert, *key); err != nil {
		log.Println(err)
		return 1
	}
	return 0
}

// review answers one AdmissionReview with a JSON patch for its Pod.
func review(body []byte, image string) ([]byte, error) {
	var in struct {
		Request *struct {
			UID    string `json:"uid"`
			Object object `json:"object"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if in.Request == nil {
		return nil, errors.New("AdmissionReview has no request")
	}
	response := object{"uid": in.Request.UID, "allowed": true}
	patch := Mutate(in.Request.Object, image)
	if len(patch) > 0 {
		raw, err := json.Marshal(patch)
		if err != nil {
			return nil, err
		}
		response["patchType"] = "JSONPatch"
		response["patch"] = raw // encoding/json base64-encodes []byte, as the API requires
		meta, _ := in.Request.Object["metadata"].(object)
		labels, _ := meta["labels"].(object)
		log.Printf("injected pod %v", labels["statefulset.kubernetes.io/pod-name"])
	}
	return json.Marshal(object{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "response": response})
}

// Mutate returns the JSON patch the three webhooks together would apply to
// pod. A Pod that already carries the proxy is left alone.
func Mutate(pod object, image string) []object {
	spec, _ := pod["spec"].(object)
	meta, _ := pod["metadata"].(object)
	if spec == nil {
		return nil
	}
	containers := objects(spec["containers"])
	for _, c := range containers {
		if c["name"] == ProxyName {
			return nil
		}
	}
	var app string
	if len(containers) > 0 {
		app, _ = containers[0]["name"].(string)
	}
	proxySecurity := object{
		"runAsUser": 1337, "runAsGroup": 1337, "runAsNonRoot": true,
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"capabilities": object{"drop": []any{"ALL"}},
	}
	resources := object{"requests": object{"cpu": "10m", "memory": "16Mi"}, "limits": object{"memory": "64Mi"}}
	initContainers := append(objects(spec["initContainers"]), object{
		"name": InitName, "image": image, "imagePullPolicy": "IfNotPresent", "args": []any{"init"},
		"securityContext": proxySecurity, "resources": resources,
	})
	containers = append(containers, object{
		"name": ProxyName, "image": image, "imagePullPolicy": "IfNotPresent", "args": []any{"sidecar"},
		"ports":           []any{object{"name": "http-envoy-prom", "containerPort": 15090}},
		"readinessProbe":  object{"httpGet": object{"path": "/healthz/ready", "port": ProxyHealthPort}, "periodSeconds": 2},
		"securityContext": proxySecurity, "resources": resources,
		"volumeMounts": []any{
			object{"name": "istio-envoy", "mountPath": "/etc/istio/proxy"},
			object{"name": "istio-podinfo", "mountPath": "/etc/istio/pod"},
		},
	})
	for _, c := range initContainers {
		decorate(c)
	}
	for _, c := range containers {
		decorate(c)
	}
	volumes := append(objects(spec["volumes"]),
		object{"name": "istio-envoy", "emptyDir": object{"medium": "Memory"}},
		object{"name": "istio-podinfo", "downwardAPI": object{"items": []any{
			object{"path": "labels", "fieldRef": object{"fieldPath": "metadata.labels"}},
			object{"path": "annotations", "fieldRef": object{"fieldPath": "metadata.annotations"}},
		}}},
		object{"name": TokenVolume, "projected": object{"sources": []any{
			object{"serviceAccountToken": object{"audience": "sts.amazonaws.com", "expirationSeconds": 86400, "path": "token"}},
		}}},
	)
	labels := copyMap(meta["labels"])
	labels["security.istio.io/tlsMode"] = "istio"
	labels["service.istio.io/canonical-revision"] = "latest"
	annotations := copyMap(meta["annotations"])
	annotations["sidecar.istio.io/status"] = `{"initContainers":["istio-init"],"containers":["istio-proxy"],"volumes":["istio-envoy","istio-podinfo"]}`
	annotations["kubectl.kubernetes.io/default-container"] = app
	annotations[InjectedAnnotation] = "true"
	return []object{
		{"op": "add", "path": "/spec/initContainers", "value": initContainers},
		{"op": "add", "path": "/spec/containers", "value": containers},
		{"op": "add", "path": "/spec/volumes", "value": volumes},
		{"op": "add", "path": "/metadata/labels", "value": labels},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
	}
}

// decorate adds IRSA and Datadog environment and the IRSA token mount to one
// container. Like the real webhooks, it never overrides a variable or mount
// the container already sets.
func decorate(c object) {
	env := objects(c["env"])
	for _, v := range []object{
		{"name": "AWS_STS_REGIONAL_ENDPOINTS", "value": "regional"},
		{"name": "AWS_ROLE_ARN", "value": RoleARN},
		{"name": "AWS_WEB_IDENTITY_TOKEN_FILE", "value": TokenDir + "/token"},
		{"name": "DD_AGENT_HOST", "valueFrom": object{"fieldRef": object{"fieldPath": "status.hostIP"}}},
		{"name": "DD_ENTITY_ID", "valueFrom": object{"fieldRef": object{"fieldPath": "metadata.uid"}}},
		{"name": "DD_ENV", "value": "integration"},
	} {
		if !slices.ContainsFunc(env, func(e object) bool { return e["name"] == v["name"] }) {
			env = append(env, v)
		}
	}
	c["env"] = env
	mounts := objects(c["volumeMounts"])
	if !slices.ContainsFunc(mounts, func(m object) bool { return m["name"] == TokenVolume || m["mountPath"] == TokenDir }) {
		mounts = append(mounts, object{"name": TokenVolume, "mountPath": TokenDir, "readOnly": true})
	}
	c["volumeMounts"] = mounts
}

func objects(v any) []object {
	if typed, ok := v.([]object); ok {
		return append(make([]object, 0, len(typed)+1), typed...)
	}
	raw, _ := v.([]any)
	out := make([]object, 0, len(raw)+1)
	for _, entry := range raw {
		if m, ok := entry.(object); ok {
			out = append(out, m)
		}
	}
	return out
}

func copyMap(v any) map[string]any {
	out := map[string]any{}
	if m, ok := v.(object); ok {
		maps.Copy(out, m)
	}
	return out
}
