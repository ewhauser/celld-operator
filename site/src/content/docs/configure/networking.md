---
title: Networking
description: Connect clients and ingress to a fleet while preserving the generated NetworkPolicy boundary.
sidebar:
  order: 4
---

Each fleet gets a ClusterIP application Service named after the fleet on TCP 8080 and a headless peer Service named FLEET-peers on TCP 8081. The operator creates both Services and a NetworkPolicy for fleet Pods. Your platform owns ingress, TLS, DNS and any client-side egress policy.

The generated policy admits application traffic only from Pods **in the same namespace** labelled celld.eric.dev/client-of: FLEET. For example, add this label to a client Deployment's Pod template:

~~~yaml
spec:
  template:
    metadata:
      labels:
        celld.eric.dev/client-of: my-fleet
~~~

Point that client's application configuration at my-fleet:8080 in the fleet namespace. If clients run in another namespace, use an ingress or proxy Pod in the fleet namespace with the label; the generated policy does not grant cross-namespace client ingress. A separate policy may be needed to allow the client's **egress**.

Peer traffic on 8081 is limited to same-fleet Pods and operator Pods in the operator namespace. The launcher port 8083 in both profiles admits only operator Pods and is not exposed by a Service. Fleet egress includes peers, kube-dns, HTTPS for S3/STS and the EKS Pod Identity Agent endpoint. The policy assumes the kube-dns Pod label k8s-app: kube-dns; check your CNI and DNS deployment.

The operator cannot determine whether the CNI enforces NetworkPolicy. The chart's networkPolicyEnforced flag is an administrator assertion after checking enforcement; provisioning stays blocked until it is true. Read [install](../../start/install/) and [security boundaries](../../reference/security-boundaries/). Do not publish the peer or launcher ports through ingress.
