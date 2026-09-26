---
title: Networking
description: Connect clients and ingress to a fleet while preserving the generated NetworkPolicy boundary.
sidebar:
  order: 4
---

Each fleet gets a ClusterIP application Service named after the fleet on TCP 8080 and a headless peer Service named FLEET-peers on TCP 8081. The peer port declares `appProtocol: tcp` because peer RPC is an opaque byte stream; service meshes that honor the standard field, such as Istio, then proxy it as TCP instead of sniffing it as HTTP. No mesh-specific configuration is required. The operator creates both Services and a NetworkPolicy for fleet Pods. Your platform supplies the Gateway or Ingress controller, TLS, DNS and any client-side egress policy. Optional `spec.routing` lets the operator manage the application route and a narrowly scoped ingress policy.

By default, the generated policy admits application traffic only from Pods **in the same namespace** labelled celld.eric.dev/client-of: FLEET. For example, add this label to a client Deployment's Pod template:

~~~yaml
spec:
  template:
    metadata:
      labels:
        celld.eric.dev/client-of: my-fleet
~~~

Point that client's application configuration at my-fleet:8080 in the fleet namespace. If clients run in another namespace, use an ingress or proxy Pod in the fleet namespace with the label; the generated policy does not grant cross-namespace client ingress. A separate policy may be needed to allow the client's **egress**.

Peer traffic on 8081 is limited to same-fleet Pods and operator Pods in the operator namespace. Fleet egress includes peers, kube-dns, HTTPS for S3/STS and the EKS Pod Identity Agent endpoint. The policy assumes the kube-dns Pod label k8s-app: kube-dns; check your CNI and DNS deployment.

PersistentFleet advertises `POD.FLEET-peers.NAMESPACE.svc:8081`, so predecessor
recovery can resolve the retained peer after its Pod IP changes. The headless
Service publishes addresses before readiness: peers may need each other's
follower service to finish startup. Do not replace these addresses with Pod IPs
or gate peer DNS on application readiness. See [recovery](../../troubleshoot/recovery/).

The operator cannot determine whether the CNI enforces NetworkPolicy. The chart's networkPolicyEnforced flag is an administrator assertion after checking enforcement; provisioning stays blocked until it is true. Read [install](../../start/install/) and [security boundaries](../../reference/security-boundaries/). Do not publish the peer port through ingress.

## Optional Gateway API routing

Install the Gateway API v1 HTTPRoute CRDs and a compatible controller. Create a
Gateway with an HTTP or HTTPS listener; for a Gateway in another namespace, its
listener's `allowedRoutes` must admit the fleet namespace. TLS certificates and
HTTPS redirects belong to the Gateway configuration. Point DNS at the Gateway.
Add this block to the fleet manifest:

```yaml
spec:
  routing:
    hostnames: [app.example.com]
    gateway:
      name: public
      namespace: edge
      sectionName: https
    source:
      namespace: edge
      podLabels:
        app: public-gateway
```

Use the actual namespace and labels of the **data-plane Pods that send traffic**,
which may differ from the controller's namespace. The operator creates
`FLEET-routing` as an HTTPRoute with a `/` prefix match to the fleet Service on
8080. The backend is always in the fleet namespace. See the
[Gateway API HTTP routing guide](https://gateway-api.sigs.k8s.io/guides/user-guides/http-routing/)
and [complete fleet samples](../../api/samples/).

## Optional Ingress routing

For an installed Ingress controller, use `ingress` instead of `gateway`:

```yaml
spec:
  routing:
    hostnames: [app.example.com]
    ingress:
      className: nginx
      tlsSecretName: app-tls
      annotations:
        cert-manager.io/cluster-issuer: letsencrypt
    source:
      namespace: ingress-nginx
      podLabels:
        app.kubernetes.io/component: controller
```

The TLS Secret is in the fleet namespace and should cover every configured
hostname. The optional cert-manager annotation works only if cert-manager and the
named issuer are already configured. Omit `tlsSecretName` for HTTP-only routing;
HTTP-to-HTTPS redirect behavior belongs to the selected controller. Class selection
uses `ingressClassName`; the legacy class annotation is rejected. Controller-specific
annotations are privileged configuration and should follow your admission policy.

Both modes require an explicit, nonempty list of hostnames and a source selector.
Wildcards such as `*.example.com` are supported. The separate `FLEET-routing`
NetworkPolicy admits only TCP 8080 from that namespace **and** those Pod labels.
The existing peer policy is unchanged. If your edge uses host
networking, SNAT or an external load balancer, verify the packet source with your
CNI and administer any additional required policy yourself; do not broaden peer
access. The operator does not discover source labels automatically.

## Updates and status

Routing is mutable. Change hostnames, class, parent, source or TLS settings in the
fleet manifest; the runtime Pods do not restart. To switch modes, replace the
whole routing block (or explicitly remove the old mode when using a merge patch).
Only one mode may be present. Remove `spec.routing` to delete the owned route and
then its ingress policy. A finalizer on an old route delays mode switching until
that resource disappears. This can cause a short routing interruption.

Read `status.conditions` for `RoutingReady`. Gateway routes wait for the configured
parent's current-generation `Accepted` and `ResolvedRefs` conditions. Missing CRDs
produce `GatewayAPIUnavailable`. Ingress reports `AwaitingAddress` until its
controller assigns an address; controllers that do not publish addresses can stay
Unknown even if traffic works. These signals are separate from `Ready` and do not
verify external connectivity. Check the Gateway listener, DNS, certificate and a
real application request before relying on the endpoint.

Resources named `FLEET-routing` must be owned by the current fleet UID; collisions
are reported, never adopted. Removed configured annotations are pruned while
unrelated controller annotations are retained. Routing changes continue during
`maintenance.paused`, which pauses runtime lifecycle work only. Fleet deletion
starts route cleanup immediately. Update the operator's per-fleet-namespace Role
when upgrading so it can manage Ingress, HTTPRoute and the additional NetworkPolicy.
