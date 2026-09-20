# Installation and operations

The chart is experimental. There must be one operator installation per cluster,
with access to all fleet namespaces. The API group is `celld.eric.dev`,
a domain the maintainer owns; it needs no DNS record. CRDs and storage reservations are
retained on uninstall; Helm does not upgrade CRDs automatically.

## Install

Build and load/publish the operator image, then install the local chart:

```sh
helm upgrade --install celld charts/celld-operator \
  --namespace celld-system --create-namespace \
  --set image.tag=YOUR_BUILT_TAG
```

The cluster role covers only the fleet API and cluster-scoped storage and node
objects. Every namespace that will hold fleets needs the namespaced Role and
RoleBinding from `config/rbac/fleet-namespace.yaml`; list them in
`fleetNamespaces` so the chart renders them, or apply the file into each
namespace by hand. A fleet in a namespace without that Role reports
`NamespaceAccessDenied` and nothing in the namespace is created. Workload and
pod deletion privileges exist only inside those namespaces, for the maintenance
executors; there is no cluster-wide delete.

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

## Leader handover and restarts

The manager runs with `LeaderElectionReleaseOnCancel`. On SIGTERM -- a
`kubectl rollout restart`, a drain, or any ordinary Pod deletion -- the outgoing
leader deletes its hold on the Lease instead of letting it expire, so the
successor starts reconciling after about one `RetryPeriod` (2s default) rather
than a full `LeaseDuration` (15s default). Only a *graceful* exit releases;
a killed or partitioned leader still hands over on the ordinary expiry path.

This matters for Bucket retirement. Completing a retirement requires positively
reading a retired writer's `nodes/<uid>.json` while its lease is already expired,
and the pinned runtime's `dead_node_gc` deletes that record a second or two after
expiry. A controller that spends fifteen seconds waiting to become leader can miss
that window entirely, which blocks Bucket contraction for that fleet permanently
(see [bucket scale-in](bucket-scale-in.md) and
[ADR 0016](decisions/0016-bucket-preflight-and-completion-boundary.md)).

Two properties make the option safe rather than merely faster:

- The binary ends when the manager ends. `run()` returns the result of
  `mgr.Start` and `main` exits immediately, which is the condition
  controller-runtime's own documentation attaches to this option
  ("requires the binary to immediately end when the Manager is stopped").
- The lease is released only after every runnable has already stopped.
  In `pkg/manager/internal.go`, `engageStopProcedure` registers the
  `leaderElectionCancel()` deferral *before* the goroutine that calls
  `runnables.LeaderElection.StopAndWait(...)`; that goroutine ends with
  `shutdownCancel()`, and the function blocks on `<-cm.shutdownCtx.Done()`
  before any deferral runs. No reconciler is still running when the next
  leader can acquire.

Lease timings themselves are left at controller-runtime's defaults.
[ADR 0012](decisions/0012-restart-safe-manual-lifecycle.md) puts lifecycle
authority in the journal and workload CAS, not in the Lease -- "an old leader
can only win that exact CAS once" -- so a shorter `LeaseDuration` would buy no
safety, and a shorter `RenewDeadline`/`RetryPeriod` would make a loaded
apiserver more likely to make a healthy leader drop its lease. A manager that
loses the lease exits, so that change would cause the restarts this option is
meant to make cheap.

Keep the termination grace period at or above the manager's
`GracefulShutdownTimeout` (both default to 30s). If the kubelet sends SIGKILL
first the release is skipped and handover falls back to lease expiry, which is
the behaviour before this option, not a regression.

## RetirementEvidenceLost

`RetirementEvidenceLost` means an admitted Bucket writer's `nodes/<uid>.json`
left the store before any assessment positively read its lease expired. The
condition names the writer, its generation and when the fleet last saw it.

This is terminal, not transient. Absence never resolves a retirement, and no
elapsed time, prior live lease or controller restart reconstructs the proof
([ADR 0016](decisions/0016-bucket-preflight-and-completion-boundary.md)), so
Bucket contraction for that fleet stays blocked and the operator offers no
administrative success flag. Every other blocker keeps the ordinary
`BucketRecoveryBlocked` / `MaintenanceRecoveryBlocked` reason, which does clear
on a later pass.

Expect it after the controller was leaderless or otherwise unable to reconcile
across a retirement -- the window between lease expiry and the runtime deleting
the record is about a second wide. Releasing the lease on shutdown and polling
that window every second make it much less likely, but neither can guarantee the
reading is taken. Treat the condition as a report to investigate, not something
to wait out.

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

The retained journal's own size is published as `celld_fleet_journal_bytes` —
the encoded journal as written, meaning the reservation annotation while it is
inline and the hydrated size once it is paged — and
`celld_fleet_journal_archive_pages`, the immutable ConfigMap pages behind it.
Both carry the same namespace/fleet labels as the other fleet series. The
`JournalSizeWarning` condition turns true once the journal passes half of either
budget that fails closed, 16 MiB hydrated or the 200 KiB archive index, and its
message states the measured bytes and the cap. It warns only; nothing blocks on
it. Act on it well before the cap, because a journal that reaches the cap can no
longer be written at all and the fleet then fails closed (see
[journal archives](journal-archives.md)).

Inspect `kubectl describe celldfleet NAME -n NAMESPACE`, its durable reservation,
and Events before intervening. A sticky `possibleLoss` condition requires
investigation and recovery; do not clear the journal to force progress. Retained
PVCs, object storage, reservations, and archived journal pages are recovery
assets, not temporary operator state. Force deletion or force detach cannot
establish that an old process stopped.

## Releases

Tag pushes run Go race tests, lint, generated-manifest checks, qualification
replays, collector tests, chart validation, and the launcher and controller
suites under both Linux architectures before publishing a multiarch
controller/launcher image. Each platform of the pushed digest is then executed
(version banner and launcher install), and the PersistentFleet kind suite runs
against that exact digest with `--operator-image`. Only after it passes are the
OCI chart and GitHub release published, both pointing at the digest. The image,
the chart and the checksum file are signed keyless with cosign under this
workflow's OIDC identity. Release notes carry the lifecycle journal version;
an older operator cannot read fleets touched by a newer journal, so downgrades
are unsupported.

Verify before installing:

```sh
cosign verify --certificate-identity-regexp 'https://github.com/ewhauser/celld-operator/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/ewhauser/celld-operator@sha256:DIGEST
```

Cadence: tag from `main` only when the nightly Integration matrix is green for
that commit. Every release stays a GitHub prerelease marked experimental until
the [EKS smoke suite](qualification/eks-smoke-plan.md) has run against a
release digest; that is the criterion for the first non-prerelease.
All releases are marked experimental until cloud qualification is independently
completed. Workflow implementation and local packaging do not mean an image or
chart has been published. `make chart-check` validates rendering and RBAC parity;
`hack/package-release.py` embeds immutable image references for release packaging.

Release publication refuses a version if its image, chart or GitHub release already exists. A partial publish requires investigation and a new version; rerunning cannot silently overwrite that version. The preflight fails closed on authentication or network errors.

The workflow uses the repository `GITHUB_TOKEN` with package write permission.
The `celld-operator` image and `charts/celld-operator` chart packages on GHCR are
public (set on 19 September 2026 after the first publication, which GHCR
creates private): pulls and the `cosign verify` commands need no credentials.
The first release, `v0.1.0-rc.3`, was verified anonymously this way: both
platforms listed in the index, image and chart signatures valid, chart pulled
and its values pinned to the image digest.
