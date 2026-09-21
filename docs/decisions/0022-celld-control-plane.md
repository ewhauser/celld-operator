# 0022: celld owns runtime safety

Status: accepted and implemented; deployment qualification is separate.

The operator uses a homogeneous celld fork based on upstream v0.5.1. celld owns
replication, shutdown and recovery. Its existing control-plane listener exposes
an operation- and generation-bound strict `remove-disk` shutdown result.

The launcher captures celld's positive `data_safe` result before terminating its
exact child. It independently proves child exit, inherited-lock release and
durable restart denial. The controller records that proof for its one current
operation before changing a workload or deleting a claim.

Kubernetes holds bucket ownership, current workload and claim identities,
bounded capacity-policy state, and one operation with fixed deadline and target
identities. A reservation resource-version comparison serializes requests;
a separate workload comparison serializes infrastructure effects. Status is an
informational projection. Completed operations discard runtime proof.

Unknown or failed completion blocks removal. HTTP acceptance, elapsed time,
Pod absence and exit status cannot become data safety. A timeout cannot cancel
an operation whose request may have been issued.

There are no deployed operator users to migrate. Remove the old implementation
and its compatibility paths. The controller has no AWS SDK, S3 recovery reader,
EC2 termination authority, archive paging or session-history replay.

Bucket uses temporary disk. PersistentFleet uses CSI volumes under the exact
storage contract in [current operations](../current-operation.md). New growth
uses fresh claims; retained predecessor disks are never automatically adopted.
Physical disk deletion requires its own exact-resource policy and qualification.

See [qualification](../qualification/README.md) for actual test boundaries.
