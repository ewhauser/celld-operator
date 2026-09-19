# ADR 0016 Bucket logical membership completion

Status: Accepted and implemented experimental manual execution; production automatic release-gated

For Bucket, complete an operation when the requested Kubernetes membership change
has converged, current survivors pass fresh health/capacity checks, and every
previously admitted generation outside current membership has a positively read
expired lease with no peer-log obligation. Repeat the exact observed membership
through settling. Persist positive expiry as soon as the first full assessment succeeds. Runtime GC may then remove that exact record while the controller independently continues fresh survivor and loss checks through settling; absence never resolves a retirement without the prior positive observation. Report physical process liveness as unknown.

This supersedes ADR 0015's physical-process-fencing requirement **for Bucket only**.
The pinned runtime requires S3 durability plus an ownership check before Bucket
acknowledgement, so peer-disk preservation is not its durability basis. Preserve
configuration, observed generation, exclusive storage scope and historical checks;
missing logs alone do not establish those facts. PersistentFleet is unchanged.

Journal every possible Deployment candidate before one CAS decrement. Retain
historical Bucket admissions through repeated shrink/grow. Journal version 5
prevents older binaries from ignoring this state. Manual execution is experimental;
automatic execution remains disabled against AWS until release qualification,
while the fixed local fixture tests the same automatic path.

See [the contract, source argument, execution and remaining gates](../bucket-scale-in.md).
