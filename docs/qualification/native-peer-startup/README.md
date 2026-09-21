# Delayed retained-witness recovery

The final `.2` hosted fault run reproduced acknowledged-write loss despite
stable DNS, changed Pod IPs, retained disks and unchanged PersistentFleet hosts.
`beta-0` declared bounded loss seven milliseconds after beginning predecessor
recovery; its retained witness `beta-1` started roughly one second later.
[The failure receipt](hosted-failure.json) identifies the exact job, source,
log checksum, missing acknowledgment and timestamps.

A native reproduction with the exact released `.2` macOS ARM64 binary delayed
the retained witness by two seconds. Both nodes had self-fenced after S3 lease
expiry; their disks, peer endpoints and object-store contents were preserved.
The earlier node reported healthy while the witness was still offline, wrote
one permanent loss record and recovered only 5/10 acknowledged values through
each node. Two of those ten writes were acknowledged while MinIO was paused.

The exact GitHub-built `.3` macOS ARM64 binary at source
`739f2baa87a5bfc4bfe04e317adf6d774edf8740` passes the matched schedule: the first
node stays alive but unready while its retained witness is unavailable, then both
nodes recover **10/10** acknowledged values with no loss record. A second case
keeps the witness unavailable through all bounded startup retries. Startup exits
1 without a loss record; explicitly restarting both nodes on their retained disks
then recovers **10/10** through each node. Two writes in each case were
acknowledged while MinIO was paused.

[The native receipt](result.json) records source and binary checksums, the matched
harness checksum, events, full-log paths and checksums. These are native/MinIO
checks. The [`.3` artifacts are published](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.3),
including the Linux amd64/arm64 image. The hosted four-suite Kind matrix passed
at operator source `010aca2aa797f868cbd9ceca44e7e50dcb564fe1` with that image:
[run 35549007431](https://github.com/ewhauser/celld-operator/actions/runs/35549007431).
The fault case kept the exact replacement child unready for at least two seconds
while its exact retained witness stayed stopped, then explicitly released that
witness. Both PersistentFleet Pod IPs changed under stable DNS. Both fleets
recovered **24/24** acknowledged writes. The
[hosted receipt](hosted-receipt.json) and [events](hosted-events.log) preserve
identities, job IDs, checksums and proof boundaries. Standard CI and site checks
also passed at that source. EKS/EBS qualification is separate.

The same `.3` binary also passes the launcher/controller HTTP handshake,
populated sequential Bucket full-stop recovery (28/28), concurrent Bucket full
stop (20/20), and concurrent Fleet full stop (21/21, including a peer-only write).
Every fresh node recovered all writes after every original disk was deleted.
A forced one-millisecond strict shutdown deadline returned immutable failure and
retained the disk. [Maintenance receipts](maintenance.json) include each exact
command, source boundary and log checksum.

The fix treats missing addresses and failed witness requests as inconclusive
regardless of lease age. It preserves the existing explicit-loss policy for
reachable witnesses reporting missing or incomplete fragments; it does not promise
recovery from permanently lost disks.

This is a separate defect from the stale Pod-IP addresses in the
[native peer-address comparison](../native-peer-addresses/README.md). Stable
addresses are necessary, but expired leases do not prove retained data is gone.
