# Shared-fleet preview feasibility

Research date: 2026-09-23. This is a source-backed design assessment, not an
implemented or runtime-qualified shared-preview feature.

Follow-up: the [cheap dedicated preview investigation](cheap-dedicated-previews.md)
evaluates the alternative requested after this assessment. It prioritizes small
dedicated runtimes with shared infrastructure and idle suspension. The shared
runtime limitations below still apply; the recommendation in this section is
the earlier design proposal.

## Original shared-runtime recommendation

Use a platform-managed **preview-only shared fleet** as the intended default,
with one logical deployment and stable URL per preview. Keep production on a
separate fleet. Retain a dedicated fleet only as an explicit isolation option.

The current celld release has useful primitives but cannot transparently run
multiple unmodified stateful branches as independent previews through operator
configuration alone. The next step should be a bounded runtime/deployment spike,
not further investment in the per-preview-fleet controller.

## Sources inspected

- Operator's recommended runtime: `v0.5.1-ewhauser.3`, source
  `739f2baa87a5bfc4bfe04e317adf6d774edf8740`, verified against the live GitHub tag.
- Live upstream `denoland/celld` main: `42269c121c989c65c0638ab01f368baf18a5f0df`
  (v0.5.1). The inspected `deploy.rs`, `generation.rs`, `runtime.rs`, and `js.rs`
  are identical between that upstream commit and the recommended fork commit.
- Cloudflare's September 22 announcement and current preview isolation docs.
- Open upstream issues for deployment lifecycle and permanent cell deletion.

The local celld checkout contains unrelated ongoing work. Inspection used an
archive of the pinned commit rather than those working-tree changes. No runtime
files or existing preview implementation files were changed for this research.
No executable proof of concept, memory benchmark, or live fleet test was run.

## What celld already supplies

**Multiple scripts in one process.** `DeploymentGraph::load` loads a primary
Worker plus transitive service-binding targets and queue consumers. Every script
gets its own configuration and isolate pool. A host-dispatching gateway Worker
could therefore forward requests to named co-hosted Worker services. This is a
candidate reuse of existing mechanisms, not a tested preview implementation.
[Deployment graph](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/generation.rs#L107-L233).

**Script-sensitive Durable Object IDs.** For ordinary user classes the namespace
key includes both script name and class name. A stable preview-specific script
name would give the same logical object name a different identity in each
preview, while preserving its identity across updates within one preview.
D1, KV and queues deliberately use shared namespace keys instead.
[Namespace derivation](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/js.rs#L12570-L12605).

**In-place deployment and old-generation draining.** Nodes build a replacement
application graph, adopt it, and let previous work drain. These mechanisms may
be reusable, but today they operate on the whole graph, not one preview.
[Generation adoption](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/runtime.rs#L1534-L1575).

## Concrete gaps

| Concern | Current behavior | Consequence for previews |
| --- | --- | --- |
| Application publication | `celld deploy` writes the named script pointer and then `deploy/current.json` | Deploying a preview normally replaces the shared fleet's primary app; there is no supported upload-only publication flag |
| Ingress selection | Public ingress serves the primary Worker and its assets | Different hostnames reaching the same fleet do not independently select scripts |
| DO class lookup | All scripts share a flat class registry; duplicate user class names are rejected | Two branches both exporting `Counter` cannot simply coexist, despite their different object-ID namespaces |
| Update isolation | The watcher compares the primary pointer; graph rebuild/adoption is global | Moving a named child pointer alone does not trigger normal adoption; a forced reload rebuilds all scripts |
| Other bindings | KV IDs, D1 IDs/names, queue names and R2 logical bucket names can identify shared resources | Each must be rewritten or supplied explicitly per preview; renaming the Worker alone is insufficient |
| Background work | Co-hosted service scripts do not run their own cron schedules | HTTP routing alone would silently miss scheduled behavior unless rejected or supported explicitly |
| Cleanup | Cell CLI supports listing; safe permanent deletion and deployment pruning remain open work | Removing a URL does not reclaim all state or fence delayed recovery/background work |

Evidence:
[CLI flags](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/deploy.rs#L190-L255),
[pointer publication](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/deploy.rs#L855-L917),
[ingress](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/main.rs#L2353-L2409),
[class registry](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/runtime.rs#L1104-L1112),
[duplicate-class refusal](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/runtime.rs#L1237-L1249),
[reload selection](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/main.rs#L817-L879),
[KV](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/kv.md),
[D1](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/d1.md),
[R2](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/r2.md),
[queues](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/queues.md),
[cron limitation](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/crates/celld/generation.rs#L216-L230).

## Approaches considered

### Shared graph with a gateway Worker

Generate a primary gateway with one service binding per preview, and dispatch an
allowlisted Host to the corresponding service. Give every preview a stable
script identity and rewrite resource identities. A build wrapper could alias
exported user classes to unique names and rewrite their binding declarations;
that possibility needs compatibility tests and is not a general existing feature.

The major problem is publication: ordinary deploy temporarily moves the root
pointer, so "deploy child then redeploy gateway" is unsafe even if the watcher
usually polls less often. A publisher would need to stage immutable artifacts,
publish named targets without moving the root, and publish one consistent graph.
Implementing this outside celld would couple the operator to internal storage
formats; adding a supported staging API is preferable. Global reloads, mutable
child pointers, assets/WebSockets, service references and cron need qualification.

This is a plausible restricted prototype, not a configuration-only solution or
an independent lifecycle per preview.

### Dynamic Workers and Durable Object facets

A stable gateway can load versioned code with Worker Loader without moving the
fleet's application pointer. Loaded code receives explicit capabilities, and
facets can supply durable child storage behind a supervisor.

However, Dynamic Workers start without normal deployment bindings, while facets
are subordinate objects inside a root DO rather than ordinary independently
addressed DO namespaces. Supporting an existing application's Wrangler config,
DO bindings, alarms, WebSockets and other services would require a capability
adapter. This is attractive for applications designed around the loader model,
but is not a transparent general preview facility.

[Dynamic Worker contract](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/dynamic-workers.md),
[facet contract](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/services/durable-object-facets.md).

### Native logical deployments on a shared preview fleet — recommended target

Add a bounded preview execution contract to celld and its deployment tooling:

1. Stage and publish a named preview revision without changing the primary app.
   Use immutable revision references and conditional updates; do not independently
   update a set of mutable pointers and call it an atomic graph.
2. Select a preview through a trusted hostname registry or a trusted gateway.
   Unknown and expired identities fail closed. Asset, HTTP and WebSocket dispatch
   must select the same preview revision.
3. Scope user class registration, cell dispatch and all binding identities by a
   stable preview UID. Do not embed commit SHA in durable identity: ordinary
   updates should retain preview state. A reset gets a new identity.
4. Adopt and drain revisions per preview so a bad deployment or update to one
   preview does not reload or prevent deployment of unrelated previews. Reuse
   existing generation machinery where possible, but this is a substantive
   runtime change rather than a small CRD edit.
5. Expose per-preview observed versions and failures. Operator Ready must require
   the serving revision, not just healthy pods or a successful artifact upload.
6. Deactivate the preview on expiry: remove ingress, reject new admission, stop
   background dispatch and drain/fence in-flight work. Retain data initially;
   design permanent cleanup separately with the runtime's recovery contract.

Initially support HTTP plus ordinary DOs with explicit preview-local config.
Reject unsupported binding/trigger types rather than forwarding them to shared
production resources. Extend the supported set only with isolation tests.

The developer API should reference a platform-owned fleet rather than embedding
its infrastructure. This is illustrative, not the currently implemented CRD:

```yaml
apiVersion: celld.eric.dev/v1alpha1
kind: CelldPreview
metadata:
  name: pr-42
spec:
  fleetRef:
    name: shared-previews
  applicationRef:
    name: my-app
  revision: immutable-build-reference
  ttlSeconds: 86400
```

`applicationRef` would supply approved preview bindings, and fleet/platform config
would supply the domain, TLS and capacity. The URL remains tied to preview UID.
The application/build resource schema remains to be designed; a Git commit label
alone is not a deployable artifact.

## Qualification spike before rewriting the operator

Run two unmodified copies of the same Counter application on one fleet and one
bucket, with different preview identities and hostnames. Require all of:

- Both can export `Counter` and use the same logical object name without sharing
  values; same-preview values survive an update and a node restart.
- Deploying or failing deployment B leaves A's serving version and active work
  unchanged. Concurrent publications cannot lose each other's updates.
- HTTP, assets, RPC and WebSockets stay within their selected preview; reject
  unknown hostnames and attempts to select another preview via untrusted headers.
- Remove B under load, restart a node and process delayed alarms/recovery; B
  remains unavailable and A still works. Report retained bytes separately from
  compute deactivation.
- Measure idle and active memory at 1, 10 and 50 previews, update latency, and
  noisy-neighbor effects. Shared hosting still consumes per-script isolate memory;
  it removes per-preview Kubernetes reservations, not all per-preview cost.

Use trusted internal branch code initially. celld explicitly does not claim
hostile multi-tenant safety, and a shared fleet shares node resources. Keep it
separate from production and use dedicated isolation for untrusted code.
[Runtime security boundary](https://github.com/ewhauser/celld/blob/739f2baa87a5bfc4bfe04e317adf6d774edf8740/docs/security.md#security-boundary).

## External context and remaining decisions

Cloudflare's new previews automatically separate Durable Object namespaces and
require separate resources for other data bindings. That is the behavioral
reference; it does not establish celld's implementation or security guarantees.
[Announcement](https://developers.cloudflare.com/changelog/post/2026-09-22-worker-previews/),
[resource isolation](https://developers.cloudflare.com/workers/previews/resources/).

Upstream issues [#181](https://github.com/denoland/celld/issues/181) (version
lifecycle, previews and pruning) and [#175](https://github.com/denoland/celld/issues/175)
(permanent cell deletion) were open when checked. They are requests, not shipped
capabilities or commitments. The source confirms the relevant missing contracts.

The principal scope decision is whether to extend the celld fork for transparent
stateful previews or deliberately ship only a restricted loader-based experience.
The recommendation is to prove the native stateful path first, preserving the
existing runtime safety contract and using the operator only for orchestration.
