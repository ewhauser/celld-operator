# Fixture provenance

These JSON files are **observed**, not invented schemas. They were captured on
18 September 2026 from unmodified celld v0.5.0 on Docker linux/arm64 with an
isolated MinIO bucket and the synthetic ledger application in
`hack/qualification/app`. The checked-in source captures and extraction mapping
are in `provenance.json`; the source captures' own manifests record raw/export
SHA-256 values, image revision, architecture, and redactions.

- `state.json`: a serving node's successful HTTP `/state` response.
- `node-open.json`: active node `a` before graceful termination.
- `node-sealed.json`: the exact same generation after survivor recovery.
- `node-deadline-open.json`: node `b` after exit 0 during a one-millisecond
  shutdown budget and a paused local object store; the log is still open.
- `node-bucket.json`: a real Bucket-mode node with no `log` field.

Cell/deployment identifiers were removed from HTTP captures. Node IDs and public
probe keys are synthetic-run identities, **not secrets**, and are retained to
check exact generation binding. No application database, bundle body, loss body,
credential, or peer-authentication secret is included. The ledger captures contain
only synthetic operation IDs/cell names, never customer payloads.

The negative cases in `adapter_test.go` are explicitly **synthetic mutations**:
missing/null/wrong-type fields, bad identities/protocols/states, duplicate JSON,
old timestamps, partial/looping listings, access errors, generation replacement,
prior unresolved sessions, epoch rewind, and historical epoch/bundle loss keys.
No real loss declaration was induced in these runs. Synthetic cases demonstrate
fail-closed interpretation, not runtime behavior under real data loss.
