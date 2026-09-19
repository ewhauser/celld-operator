# Critical-feature integration validation

This run integrates independent Bucket, storage/launcher, and maintenance work
with Helm packaging, release guardrails, and operational status/metrics. Changes
are local and uncommitted; this report does not establish a published release.

## Local checks

`checks.log` records the assembled build, full Go race suite, lint,
CRD/deepcopy reproducibility, Helm rendering and canonical RBAC checks, release
publication guard unit tests, offline qualification replay, and eight Python
collector tests. One initial import-format lint error was corrected; the later
lint invocation reports zero issues. Offline expected blocks are successful
negative tests, not successful live removal evidence.

The integration review added regressions for Ordered Bucket initial provisioning
and expansion without PVCs, maintenance preservation of disk/loss fences,
scheduling-gate release during replacement, stale restart admission, pause
resumption, signed host boot identity, and permanent retired-Pod restart denial.
Archive tests exercise thousands of retained records, immutable page reuse,
missing/tampered/foreign pages, and publication conflicts without history pruning.

## Live Ordered Bucket evidence

The [Ordered Bucket run](../ordered-bucket/README.md) passed gated three-pod
startup across two simulated AZs, exact host/zone assignment without PVCs,
highest-ordinal 3-to-2 removal, all 12 acknowledged writes, same-Pod container
restart with generation succession, growth, operator restart, and a second
contraction. The test cluster was deleted and its temporary inotify setting was
restored. The test build contained Ordered/succession and provisioning fixes;
subsequent steady-state admission is covered by local regression tests and the
assembled maintenance build.

## Maintenance live test boundary

The assembled maintenance run could not reach application testing. Pulling the
exact pinned celld image into the first owned Kind node exceeded the 300-second
network timeout. No operator or runtime was deployed by that attempt, so it
establishes neither a maintenance pass nor a maintenance failure. The cluster was
cleaned and the temporary inotify setting restored. See the [bounded report](../critical-maintenance/README.md).
Rolling restart and final shutdown have race-tested controller/protocol coverage;
positive live qualification remains outstanding.

## External boundaries

Unit and local Kind evidence cannot qualify EKS/S3/EBS. Real CSI attachment
handoff, uncertain-node failures, production automatic contraction, follower AZ
diversity, and cross-version transitions remain outside this run. No cloud
infrastructure was changed, no image/chart was published, and no commit was made.
The [current checklist](../../critical-features.md) separates remaining runtime
implementation dependencies from those qualification gates.
