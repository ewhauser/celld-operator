# ADR 0016 Bucket preflight and the process-completion boundary

Status: Implemented admission prerequisites; Bucket executor remains blocked

Treat the pinned Bucket acknowledgement rule separately from PersistentFleet
peer-log recovery. Inspect every possible Deployment victim and require complete,
fresh no-peer-log metadata for the narrow Bucket-only admission case. An absent
log is not proof of process termination, historical recovery, or Bucket runtime
configuration. Return a distinct preflight observation rather than a completed
peer recovery certificate.

Preserve ADR 0015's stronger completion and identity contract. Do not enable a
removal executor until that authority is qualified or a different Bucket
completion contract is explicitly adopted and validated. Deterministic Deployment
victim selection is unnecessary when every candidate can be admitted safely.

See [the implementation and outstanding work](../bucket-scale-in.md).
