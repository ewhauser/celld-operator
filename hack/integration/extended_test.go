package main

import "testing"

func TestSweptReadsSeparatesLostWrites(t *testing.T) {
	out := "READ load-1 w1-1\nREAD load-2 w1-2\nMISSING load-3 w1-3\nSWEPT\n"
	readable, missing, err := sweptReads(out, 3)
	if err != nil || readable != 2 || !same(missing, []ledgerEntry{{"load-3", "w1-3"}}) {
		t.Fatalf("readable=%d missing=%v err=%v", readable, missing, err)
	}
	for name, out := range map[string]string{
		"no summary":  "READ load-1 w1-1\nREAD load-2 w1-2\nMISSING load-3 w1-3\n",
		"short":       "READ load-1 w1-1\nSWEPT\n",
		"exec failed": "error: unable to upgrade connection\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := sweptReads(out, 3); err == nil {
				t.Fatalf("incomplete sweep accepted: %q", out)
			}
		})
	}
}

func TestParseLossRecords(t *testing.T) {
	listing := `[2026-09-25 10:00:00 UTC]   312B STANDARD log/pair-0/0192.e3.loss.json
[2026-09-25 10:00:00 UTC]  1.2KiB STANDARD log/pair-0/0192.e3.bundle
[2026-09-25 10:00:01 UTC]   300B STANDARD nodes/pair-0.json
[2026-09-25 10:00:02 UTC]   298B STANDARD log/pair-1.e12.loss.json
`
	got := parseLossRecords(listing)
	if !same(got, []string{"log/pair-0/0192.e3.loss.json", "log/pair-1.e12.loss.json"}) {
		t.Fatalf("loss records = %v", got)
	}
}

func TestInjectedShapesRequiresEveryContainer(t *testing.T) {
	container := func(name string) object {
		return object{"name": name, "env": []any{
			object{"name": "AWS_ROLE_ARN"}, object{"name": "AWS_WEB_IDENTITY_TOKEN_FILE"},
			object{"name": "DD_AGENT_HOST", "valueFrom": object{"fieldRef": object{"fieldPath": "status.hostIP"}}},
			object{"name": "DD_ENTITY_ID"}, object{"name": "DD_ENV"},
		}, "volumeMounts": []any{object{"name": "aws-iam-token", "mountPath": "/var/run/secrets/eks.amazonaws.com/serviceaccount"}}}
	}
	pod := object{
		"metadata": object{"name": "mesh-0", "annotations": object{"admission.celld.eric.dev/injected": "true"}, "labels": object{"security.istio.io/tlsMode": "istio"}},
		"spec": object{
			"initContainers": []any{container("istio-init")},
			"containers":     []any{container("celld"), container("istio-proxy")},
			"volumes":        []any{object{"name": "aws-iam-token", "projected": object{"sources": []any{object{"serviceAccountToken": object{"audience": "sts.amazonaws.com"}}}}}},
		},
		"status": object{"containerStatuses": []any{object{"name": "istio-proxy", "ready": true}}},
	}
	if err := injectedShapes(pod); err != nil {
		t.Fatal(err)
	}
	pod["spec"].(object)["containers"] = []any{object{"name": "celld"}, container("istio-proxy")}
	if err := injectedShapes(pod); err == nil {
		t.Fatal("a container without injected environment was accepted")
	}
}
