# Installation and operations

The chart is experimental. There must be one operator installation per cluster,
with access to all fleet namespaces. The API group remains `celld.example.com`
until an owned release domain is selected. CRDs and storage reservations are
retained on uninstall; Helm does not upgrade CRDs automatically.

## Install

Build and load/publish the operator image, then install the local chart:

```sh
helm upgrade --install celld charts/celld-operator \
  --namespace celld-system --create-namespace \
  --set image.tag=YOUR_BUILT_TAG
```

Use an immutable `image.digest` and matching `launcherImage` for PersistentFleet.
Published release charts embed both as the same immutable image digest. The
launcher binary is injected without modifying the celld image. Before enabling
`networkPolicyEnforced`, independently establish that the cluster CNI enforces
the generated policy. This flag is an operator assertion, not a CNI installer.
Runtime identity, storage credentials, StorageClass, and Metrics Server are
external prerequisites; refer to the profile and qualification documents.

Before upgrading the controller, back up fleet specifications, retained reservations, immutable journal archive
ConfigMaps, and launcher credential Secrets together (see [journal archives](journal-archives.md)), inspect CRD changes, and explicitly apply `config/crd/`. Never
roll back to an operator that cannot read the persisted journal version. A
controller release is distinct from a celld runtime upgrade; only registered,
qualified runtime adapters may authorize transitions.

## Monitoring

`metrics.enabled=true` exposes port 8084 through a ClusterIP service.
`metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` require
Prometheus Operator CRDs already installed. The chart optionally alerts on
stalled operations, possible loss, and fleets with zero ready replicas.
Restrict metrics access using cluster network policy as appropriate.

Fleet status distinguishes desired, applied, observed, ready, joining, and
terminating replicas. `replicaObservationValid` indicates a complete Pod list;
incomplete inventory must not be interpreted as zero running processes.
`blockedSince` and lifecycle start/completion timestamps expose wait duration.
The operator emits Events only when the blocker status or reason changes.
Prometheus series are labeled by namespace/fleet and removed when deletion is
observed. These operational observations never substitute for fencing evidence.

Inspect `kubectl describe celldfleet NAME -n NAMESPACE`, its durable reservation,
and Events before intervening. A sticky `possibleLoss` condition requires
investigation and recovery; do not clear the journal to force progress. Retained
PVCs, object storage, reservations, and archived journal pages are recovery
assets, not temporary operator state. Force deletion or force detach cannot
establish that an old process stopped.

## Releases

Tag pushes run Go race tests, lint, generated-manifest checks, qualification
replays, collector tests, and chart validation before publishing a multiarch
controller/launcher image and OCI chart. The release workflow records the image
digest, includes CRDs and checksums, and verifies the chart pulled from GHCR.
All releases are marked experimental until cloud qualification is independently
completed. Workflow implementation and local packaging do not mean an image or
chart has been published. `make chart-check` validates rendering and RBAC parity;
`hack/package-release.py` embeds immutable image references for release packaging.

Release publication refuses a version if its image, chart or GitHub release already exists. A partial publish requires investigation and a new version; rerunning cannot silently overwrite that version. The preflight fails closed on authentication or network errors.

The workflow uses the repository `GITHUB_TOKEN` with package write permission. First publication defaults to private visibility; make an explicit visibility decision or configure pull credentials before installation ([GitHub Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)).
