---
title: Send your first application request
description: Deploy the repository's small Durable Object app into your fleet bucket, then write and read through port 8080.
sidebar:
  order: 5
---

A ready fleet serves an application deployed into **that fleet's S3 bucket**. This guide uses [hello-world](https://github.com/ewhauser/celld-operator/tree/main/examples/hello-world), a small Worker that stores a named ID in a Durable Object and returns JSON.

Use a verified native CLI from the [compatible fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.2). This Worker also needs the `esbuild` executable on `PATH`; with Node.js and npm installed, run `npm install --global esbuild@0.25.12` and check `esbuild --version`. The pinned [deployment instructions](https://github.com/ewhauser/celld/blob/main/docs/README.md#deploy-an-application) describe the CLI flags and Wrangler support. Use an approved AWS profile that can deploy to the fleet bucket.

## Deploy the app

From the repository root, select your AWS profile and deploy to the **same bucket** named in `my-fleet.yaml`. `celld deploy` writes the deployment objects to S3; the running fleet loads the latest successful deployment. Check your selected account before writing:

```bash
aws sts get-caller-identity --profile YOUR_AWS_PROFILE
AWS_PROFILE=YOUR_AWS_PROFILE celld deploy ./examples/hello-world \
  --bucket s3://YOUR_DEDICATED_BUCKET \
  --region YOUR_AWS_REGION
```

The deployment identity has bucket write permissions; the operator itself needs no AWS role. No credentials are copied into the fleet manifest. Deploying a different app to this bucket replaces the fleet's current application, so use a dedicated nonproduction bucket for this guide.

## Write and read through the fleet

For a quick local check, forward only the application Service to your machine. This command does not expose the peer port:

```bash
kubectl --context YOUR_CONTEXT -n fleets port-forward service/my-fleet 8080:8080
```

Leave that terminal running. celld checks the deployment pointer about every 30 seconds, so the app may not serve requests immediately after `celld deploy` exits. In a second terminal, use a bounded **GET** check before sending the first write. A fresh app returns `"stored":false`; a previous run with the same ID may return `true`.

```bash
app_ready=false
for attempt in {1..12}; do
  if curl --fail --silent --show-error --max-time 5 \
    'http://127.0.0.1:8080/?cell=quickstart&id=hello' |
    grep -Eq '"id":"hello","stored":(true|false)'; then
    app_ready=true
    break
  fi
  sleep 5
done
if [ "$app_ready" = true ]; then
  curl --fail-with-body -X PUT 'http://127.0.0.1:8080/?cell=quickstart&id=hello'
  curl --fail-with-body 'http://127.0.0.1:8080/?cell=quickstart&id=hello'
else
  echo 'App did not become available; inspect the deployment and runtime logs.' >&2
fi
```

Both responses after the availability check should include `"stored":true` for `id=hello`. The PUT writes the Durable Object state once; the final GET reads it. Do not automatically retry a PUT whose outcome is uncertain. If the Service has no endpoint, return to [fleet verification](../verify/). If the request reaches celld but the app fails, check the deployment output and runtime Pod logs. For an in-cluster client or ingress, label its Pod `celld.eric.dev/client-of: my-fleet` in the `fleets` namespace; see [networking](../../configure/networking/).

This confirms one application request through your fleet. It does not qualify EKS, S3 failure behavior, recovery or production durability; those gates are listed under [capability and qualification limits](../../reference/limitations/).
