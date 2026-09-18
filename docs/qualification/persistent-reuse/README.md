# Persistent retained-volume experiment

18 September 2026. Local Docker/MinIO, unchanged celld v0.5.0 digest
`sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8`.

The old invocation was alive and paused with its mount retained. A replacement
on the same named volume acquired a new S3 generation and became Ready in 1.077
seconds. The records and result here are copied from the completed real run;
no fields were synthesized. Node names, addresses, and IDs are synthetic fixture
values. No application payload, peer-auth secret or credential is included.

Script SHA-256: `0fe89463437a9cbcacdcebb783f51760495aadb910048696d19007663778eb0f`.

Raw local output: `.qualification-runs/persistent-reuse-overlap-captured/`.
Both experiment runs cleaned their own containers, volumes, and network.
The process was not resumed against the replacement. No peer-only acknowledged
writes, data loss, EKS, EBS or forced Kubernetes deletion were tested.

Checks run after this investigation: `make check` (build, race tests, lint),
`make manifests-check`, `make qualification-replay` (five positive candidate
captures and four expected blocks), `make qualification-test` (eight tests), and
`git diff --check`, all passed. Go tests are regression checks of unchanged
controller code; they are not new PersistentFleet execution coverage. No kind
integration was rerun because no controller or manifest behavior changed.

See [the implementation gap and source analysis](../../persistent-fleet-implementation-gap.md).
