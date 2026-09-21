# Released native fork: coordinated maintenance

On September 20, 2026, the released `0.5.1-ewhauser.2` macOS ARM64 binary
passed three populated full-stop schedules against isolated Docker MinIO:

| Schedule | Acknowledged writes recovered through every fresh node |
| --- | --- |
| Bucket sequential 3→2→1→0; last member held eight resident cells | 28/28 |
| Bucket concurrent stop of all three nodes | 20/20 |
| Fleet concurrent stop, including a peer-only acknowledgment while MinIO was paused | 21/21 |

Every original disk was deleted only after its exact runtime generation reported
`data_safe` and its process exited successfully. All three replacement nodes
started on fresh disks. No loss markers were observed. A separate one-millisecond
deadline produced an immutable failure and retained the affected disk.

Version `.1` failed the same populated sequential Bucket schedule because its
last member waited for successor adoption. Version `.2` completes strict disk
removal after durability, runtime stop and ownership release, without requiring
a successor. Ordinary handoff retains its adoption behavior.

[The result](result.json) records the source commit, exact release-binary hash and
terminal outcomes. [Success events](events.json) and [deadline events](deadline-events.json)
record the observations. The [maintained native harness](https://github.com/ewhauser/celld/blob/v0.5.1-ewhauser.2/examples/disk-removal/demo.py)
lives in the fork; complete local evidence is recorded in the result. Release
checksums and GitHub attestations were verified for all three native platforms.

This establishes native celld/MinIO recovery through full disk replacement. It
does not establish Kubernetes sequencing, launcher exclusion, sustained stress
behavior or EKS/EBS qualification. The operator's maintained integration suite
is `go run ./hack/integration --suite all`.
