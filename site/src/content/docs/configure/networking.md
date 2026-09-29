---
title: Networking
description: Connect clients and ingress to a fleet while preserving the generated NetworkPolicy boundary.
sidebar:
  order: 4
---

Each fleet gets a ClusterIP application Service named after the fleet on TCP 8080 and a headless peer Service named FLEET-peers on TCP 8081. The peer port declares `appProtocol: tcp` because peer RPC is an opaque byte stream; service meshes that honor the standard field, such as Istio, then proxy it as TCP instead of sniffing it as HTTP. Sidecars injected into a PERMISSIVE mesh need nothing more; for STRICT mTLS or Istio authorization, use [`spec.mesh.istio`](#istio-strict-mtls-and-authorization). The operator creates both Services and a NetworkPolicy for fleet Pods. Your platform supplies the Gateway or Ingress controller, TLS, DNS and any client-side egress policy. Optional `spec.routing` lets the operator manage the application route and a narrowly scoped ingress policy.

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

## Istio STRICT mTLS and authorization

Without mesh settings, a fleet's generated NetworkPolicy has no route to istiod,
the operator's plaintext `/state` reads on 8081 fail under STRICT mTLS, and the
first ALLOW AuthorizationPolicy that selects the members denies peer RPC,
because nothing allows port 8081. Set `spec.mesh.istio` when you create the
fleet:

```yaml
spec:
  mesh:
    istio:
      controlPlaneNamespace: istio-system # default
      applicationAccess: Policies         # default AllowAll
```

The operator then:

- labels the Pod template `sidecar.istio.io/inject: "true"` and
  `celld.eric.dev/fleet: FLEET`, and requests a native sidecar, which starts
  before celld and stops after it so the SIGTERM handoff to peers still has a
  proxy;
- excludes the EKS Pod Identity agent address from the proxy;
- adds NetworkPolicy egress to istiod Pods labelled `app: istiod` in
  `controlPlaneNamespace` on TCP 15012;
- creates the ALLOW AuthorizationPolicy `FLEET-mesh`, which admits port 8081 only
  from the fleet's ServiceAccount and the operator's. With `AllowAll` it also
  allows port 8080 from any caller, as outside the mesh. With `Policies`, Istio
  denies port 8080 until an AuthorizationPolicy of yours allows the caller.

Select members in your own policies with `celld.eric.dev/fleet: FLEET`; see the
[istio-mesh sample](../../api/samples/#istio-mesh). NetworkPolicy still applies
underneath: callers need the `celld.eric.dev/client-of` label or `spec.routing`
as above. A DENY policy of yours that matches port 8081 overrides the operator's
ALLOW and can break the fleet.

The operator reads each member's `/state` on 8081 directly, so under STRICT mTLS
its own Pods must be in the mesh. Set the chart value
`podLabels."sidecar.istio.io/inject": "true"` (or label the operator namespace
for injection); the chart passes the operator's ServiceAccount to
`--operator-service-account`, which the fleet policy admits. With an operator
outside the mesh, capacity samples and application status go missing. Revision
installs must label the fleet namespace with `istio.io/rev`, since the pod label
alone selects the default revision. Under a `REGISTRY_ONLY` outbound policy,
add ServiceEntries for S3, STS and your collector.

`controlPlaneNamespace` and `applicationAccess` are mutable and change only
policies. Joining or leaving the mesh changes every member Pod, and under STRICT
mTLS a fleet that is half in the mesh cannot reach its own peers, so membership
changes only across a full stop, as in the
[full-stop upgrade](../../operate/upgrade-runtime/#full-stop-upgrade). The API
accepts a membership change only on a paused fleet, and the operator reports
`MeshTransitionBlocked` and keeps the old template while any member Pod remains.
Members keep their disks. To move an existing fleet into the mesh:

```bash
CONTEXT=YOUR_CONTEXT
NAMESPACE=fleets
FLEET=my-fleet
FLEET_UID=$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get celldfleet "$FLEET" -o jsonpath='{.metadata.uid}')

# 1. Pause the fleet.
kubectl --context "$CONTEXT" -n "$NAMESPACE" patch celldfleet "$FLEET" --type merge \
  -p '{"spec":{"maintenance":{"paused":true}}}'
kubectl --context "$CONTEXT" -n "$NAMESPACE" wait "celldfleet/$FLEET" \
  --for=condition=MaintenancePaused --timeout=2m

# 2. Stop every member (use deployment for a Bucket fleet with the Deployment
#    layout), and wait until no member Pod remains.
kubectl --context "$CONTEXT" -n "$NAMESPACE" scale statefulset "$FLEET" --replicas=0
while kubectl --context "$CONTEXT" -n "$NAMESPACE" get pods \
  -l "celld.eric.dev/fleet-uid=$FLEET_UID" -o name | grep -q .; do sleep 5; done

# 3. Join the mesh and resume in one change. Every member returns in the mesh
#    at the declared count.
kubectl --context "$CONTEXT" -n "$NAMESPACE" patch celldfleet "$FLEET" --type merge \
  -p '{"spec":{"mesh":{"istio":{}},"maintenance":{"paused":false}}}'
kubectl --context "$CONTEXT" -n "$NAMESPACE" wait "celldfleet/$FLEET" \
  --for=condition=Ready --timeout=20m
```

To leave the mesh, run the same steps with `"mesh":null` in step 3. The
`FLEET-mesh` policy stays until the fleet is deleted; without a sidecar it has
no effect. Update the
per-fleet-namespace Role when upgrading: it now grants `get`, `create` and
`update` on `security.istio.io` AuthorizationPolicies. The operator requires
Istio's `security.istio.io/v1` API and reports `InfrastructureBlocked` without
it. Native sidecars need Kubernetes 1.29 or later.

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
