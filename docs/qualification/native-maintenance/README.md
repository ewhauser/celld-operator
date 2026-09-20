# Released native fork: coordinated maintenance

On September 20, 2026, the released `0.5.1-ewhauser.1` macOS ARM64 binary
completed strict 3→2→1→0 shutdown against isolated Docker MinIO. Each exact
generation returned `data_safe`; its process then exited before its test disk
was deleted. All three nodes restarted on fresh disks and recovered all 29
acknowledged writes through every node. After eight further writes, every node
verified 37/37 entries in both Durable Object KV and SQL.

The run included an acknowledgment while MinIO was paused, exercising peer-disk
durability. A separate one-millisecond deadline returned an immutable failure
and retained the affected disk. No loss markers were observed.

[The result](result.json) records the binary/source hashes, exact command,
harness hashes and terminal results. [Success events](events.json) and
[deadline events](deadline-events.json) record the observations. The independent
harness and complete evidence remain in the local artifact directory recorded
in the result; this was an additional bounded qualification run, not a CI gate.
The repository's maintained end-to-end suite is `go run ./hack/integration --suite all`.

This run establishes native celld/MinIO recovery through full disk replacement.
It does not establish Kubernetes sequencing, launcher exclusion, sustained
stress behavior or EKS/EBS qualification; those have separate tests.
