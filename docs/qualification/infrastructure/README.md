# Step 2 local infrastructure evidence

18 September 2026. Base commit: `83d1627` (reviewed step 1).
All changes remain uncommitted for parent review. This is **local experimental
infrastructure validation, not AWS or production qualification**.

## Executed checks

- `make check`: build, all Go race tests and golangci-lint v2.13.2 (zero issues).
  API tests cover defaults and invalid configurations. Reconciliation tests cover
  both profiles, cross-namespace and concurrent reservation conflicts, a new UID
  attempting to reclaim storage, missing dependencies, unsafe mutations, drift,
  missing workloads, a crash after durable creation intent, and observed readiness.
- `make manifests-check`: generated CRDs and deepcopy artifacts match pinned
  controller-gen output. The kind API server also accepts the CRDs and manager
  manifest, applies defaults, and rejects invalid and immutable-spec updates.
- `make qualification-replay`: five positive candidate assessments and four
  expected blocks from the step 1 captures.
- `make qualification-test`: all eight collector fault/completeness tests pass.
- `CELLD_DOCKER_TEST=1 go test -race ./internal/controller -run TestLauncher -v`:
  the actual pinned linux/arm64 runtime image waits a full TTL before exec (10.16 seconds
  including Docker overhead), and TERM during the delay exits 0 without launching
  celld. The [captured launcher run](launcher.log) passed both checks.
  The two network-disabled containers are removed. This is not a process
  fencing, node-partition or Kubernetes restart-recovery qualification.
- `python3 hack/integration/run.py`: unique disposable kind cluster, Kubernetes
  1.31.4, Calico 3.29.3 with checked manifest SHA, MinIO and local-path Retain PVCs.
  The operator runs as a native process using the exact installed ServiceAccount
  RBAC, never the default kubeconfig. The runtime is the unchanged v0.5.0 digest
  pinned by step 1. The qualification app is independently deployed to two buckets.
- `actionlint .github/workflows/ci.yaml`, Python compilation and `git diff --check`.

## Live integration assertions

The [captured successful run](kind.log) records:

1. A two-replica Bucket Deployment and a two-replica PersistentFleet StatefulSet
   become Ready with the real runtime. No readiness gate is bypassed.
2. Pods stay in the configured eligible zone. A separate strict two-zone fleet
   leaves a replica unschedulable when one configured zone is unavailable.
3. Same-fleet and correctly namespaced operator probes can reach `/state`.
   Other-fleet peers, ordinary clients and untrusted pods cannot reach the private
   port. An authorized client reaches application health through the ClusterIP
   Service; the other fleet's application endpoint denies that client.
4. A write and read in the first fleet succeeds; the same key is absent from the
   second fleet's independent storage scope. This is a basic isolation assertion,
   not a correlated-failure durability experiment.
5. A second namespace cannot provision a fleet against the first fleet's bucket.
   Invalid AZ/storage combinations and spec mutations are rejected by admission.
6. Fleet deletion remains blocked while the original StatefulSet and both PVCs
   remain. No workload update or deletion privilege is granted to the operator.
7. After controller termination, the test clears only alpha's status and restarts
   the controller. Readiness is reconstructed from the durable reservation and
   existing workload, whose UID is unchanged.

The harness removes only its own named cluster and process in a finally block.
The pre-existing `cursor-e2e` cluster is untouched. No AWS resources, identities,
buckets, clusters or default/pre-existing Kubernetes contexts are used.

## Issues found and fixed during validation

A readiness-probe API default (`successThreshold: 1`) initially appeared as drift.
A failing regression test was added before explicitly setting the default; the
retest passed. Reads now decode into empty objects, rather than prepopulated
expected specs that could conceal absent fields.

Harness setup exposed a second-node open-file limit in the local Docker VM,
Docker/kind multi-architecture archive import incompatibility, and an unusable
node `/tmp` mount for copied app artifacts. Test clusters from these attempts were
removed. The harness uses one node, pulls directly through its CRI, and places
its temporary app files under `/opt` inside that disposable node. No host limits
or pre-existing cluster resources were changed. Test probes have independent
controller ownership and remain unready for application routing, so ReplicaSets
cannot adopt them or Services accidentally route application traffic to them.

## Remaining limits

Successful multi-node/multi-AZ spread is **not tested** here. Strict missing-zone
behavior and generated scheduling rules are tested; actual EKS capacity, EBS
attachment/recovery, IAM/Pod Identity/IRSA and CNI behavior still require the
[recorded AWS prerequisites](../README.md). Pod spread does not prove follower
AZ diversity. Local-path PVC retention is not retained-EBS recovery.

The API group is explicitly provisional. Specs are immutable, including additive
replica changes. Initial provisioning is useful, but scale-in, rolling updates,
automatic teardown, controlled restarts and production enablement remain blocked.
Reservations never automatically expire or transfer. Ordinary kubelet/workload
controller replacement after external failures is not fenced by this prototype;
PVC loss/recreation and unreachable old processes remain qualification blockers.
NetworkPolicy depends on verified CNI enforcement and trusted namespace/label
administration; other additive policies or privileged actors can defeat it.

The offline S3 evidence adapter is not connected to lifecycle decisions. No result
here changes the step 1 limits on Bucket no-log interpretation, peer-only recovery,
loss discovery, process fencing, sustained-pressure scale-out or AWS durability.

## Review follow-up

Three regressions were reproduced before fixing them:

- Initial StatefulSet creation accepted an existing ordinal PVC from another fleet.
  It now rejects all pre-existing claims and atomically acquires claim names before
  creating the StatefulSet. Race and partial-allocation tests verify that no
  workload starts and every allocated claim remains retained.
- Subset comparison accepted additional liveness probes, duplicate bucket
  environment overrides, sidecars, init containers and volumes. Complete spec
  comparison now normalizes only known API defaults and allocated Service addresses;
  tests cover both accepted defaults and rejected additional configuration.
- A concurrent finalizer addition was overwritten by an unconditional merge patch.
  An optimistic resource-version patch now returns a conflict, and retry preserves
  both finalizers.

The [follow-up kind run](review-fixes-kind.log) also checks pre-existing claim
rejection, real API defaults, and detection of an added liveness probe and bucket
override. That drift test uses OnDelete, verifies unchanged running Pod UIDs,
and restores the original template before checking readiness again.
