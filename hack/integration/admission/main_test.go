package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func pod() object {
	var p object
	if err := json.Unmarshal([]byte(`{
	  "metadata": {"generateName": "mesh-", "labels": {"celld.eric.dev/fleet-uid": "u"}},
	  "spec": {
	    "containers": [{"name": "celld", "image": "celld", "env": [{"name": "AWS_REGION", "value": "us-east-1"}, {"name": "DD_ENV", "value": "prod"}], "volumeMounts": [{"name": "data", "mountPath": "/work"}]}],
	    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "data-mesh-0"}}]
	  }
	}`), &p); err != nil {
		panic(err)
	}
	return p
}

func patched(t *testing.T, ops []object) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, op := range ops {
		if op["op"] != "add" {
			t.Fatalf("unexpected op %v", op)
		}
		out[op["path"].(string)] = op["value"]
	}
	return out
}

func env(c object) map[string]object {
	out := map[string]object{}
	for _, e := range objects(c["env"]) {
		out[e["name"].(string)] = e
	}
	return out
}

func TestMutateInjectsEveryShape(t *testing.T) {
	p := patched(t, Mutate(pod(), "img"))
	containers := p["/spec/containers"].([]object)
	inits := p["/spec/initContainers"].([]object)
	if len(containers) != 2 || containers[0]["name"] != "celld" || containers[1]["name"] != ProxyName {
		t.Fatalf("containers = %v", containers)
	}
	if len(inits) != 1 || inits[0]["name"] != InitName {
		t.Fatalf("init containers = %v", inits)
	}
	for _, c := range append(containers, inits...) {
		e := env(c)
		for _, name := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "DD_AGENT_HOST", "DD_ENTITY_ID", "DD_ENV"} {
			if _, ok := e[name]; !ok {
				t.Fatalf("%s lacks %s", c["name"], name)
			}
		}
		mounted := false
		for _, m := range objects(c["volumeMounts"]) {
			mounted = mounted || (m["name"] == TokenVolume && m["mountPath"] == TokenDir)
		}
		if !mounted {
			t.Fatalf("%s lacks the IRSA token mount", c["name"])
		}
	}
	// Existing variables are never overridden or duplicated.
	celld := env(containers[0])
	if celld["DD_ENV"]["value"] != "prod" || len(objects(containers[0]["env"])) != 7 {
		t.Fatalf("celld env = %v", containers[0]["env"])
	}
	volumes := p["/spec/volumes"].([]object)
	if len(volumes) != 4 || volumes[0]["name"] != "data" || volumes[3]["name"] != TokenVolume {
		t.Fatalf("volumes = %v", volumes)
	}
	if p["/metadata/labels"].(map[string]any)["celld.eric.dev/fleet-uid"] != "u" || p["/metadata/annotations"].(map[string]any)[InjectedAnnotation] != "true" {
		t.Fatalf("metadata = %v %v", p["/metadata/labels"], p["/metadata/annotations"])
	}
}

func TestMutateIsIdempotent(t *testing.T) {
	p := pod()
	p["spec"].(object)["containers"] = append(p["spec"].(object)["containers"].([]any), object{"name": ProxyName})
	if ops := Mutate(p, "img"); ops != nil {
		t.Fatalf("already injected Pod patched again: %v", ops)
	}
}

func TestReviewAnswersWithBase64JSONPatch(t *testing.T) {
	body, _ := json.Marshal(object{"request": object{"uid": "abc", "object": pod()}})
	out, err := review(body, "img")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Response struct {
			UID, PatchType, Patch string
			Allowed               bool
		}
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(r.Response.Patch)
	if err != nil || r.Response.UID != "abc" || !r.Response.Allowed || r.Response.PatchType != "JSONPatch" {
		t.Fatalf("response = %s", out)
	}
	var ops []object
	if err := json.Unmarshal(raw, &ops); err != nil || len(ops) != 5 {
		t.Fatalf("patch = %s", raw)
	}
}
