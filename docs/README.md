# Celld operator documentation

Published at [ewhauser.github.io/celld-operator](https://ewhauser.github.io/celld-operator/).
The site syncs every file here at build time; see [site/README.md](../site/README.md).

## Current state

- [Critical feature checklist](critical-features.md): what executes today, what is a
  runtime dependency, and what remains an external qualification gate. This is the
  authoritative feature matrix; older step documents are historical snapshots.
- [Fleet API and installation](fleet-api.md), [operations and Helm](operations.md).
- Contracts by area: [Bucket contraction](bucket-scale-in.md),
  [Ordered Bucket placement](ordered-bucket.md), [Bucket migration](bucket-migration.md),
  [PersistentFleet launcher lifecycle](persistent-fleet-lifecycle.md),
  [EC2 fencing](infrastructure-fencing.md), [maintenance execution](maintenance-execution.md),
  [runtime versions](runtime-versions.md), [capacity policy](capacity-policy.md),
  [journal archives](journal-archives.md) (current journal version and compatibility),
  [runtime dependencies](runtime-dependencies.md).
- [Architecture decision records](decisions/README.md).
- Evidence: [qualification index](qualification/README.md), [local harness](../hack/qualification/README.md),
  [fault injection in kind](qualification/faults/README.md).

## Historical investigations and step records

These describe the state at the step they were written and are kept for the
reasoning they contain. Where they say a path is blocked or name a journal
version, check the current-state documents above.

- [Runtime qualification](runtime-qualification.md), [shutdown evidence](shutdown-evidence.md),
  [read-only S3 recovery evidence](s3-recovery-evidence.md): the original investigations.
- [Shared lifecycle safety](shared-lifecycle-safety.md) (step-3 prerequisites),
  [PersistentFleet implementation gap](persistent-fleet-implementation-gap.md)
  (analysis that motivated the launcher), [pre-release review](pre-release-review.md)
  (review at commit `d3ea0df`).
