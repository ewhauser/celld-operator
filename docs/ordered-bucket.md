# Ordered Bucket fleets

New Bucket fleets may set `spec.bucketWorkload: Ordered` to use a StatefulSet
with disk-backed `emptyDir`. Runtime node identity remains the Pod UID, not the
StatefulSet ordinal. No PVC or retained local identity is introduced.
See the [two-AZ example](../config/samples/bucket-ordered.yaml); provision runtime
credentials, service account and network policy enforcement before applying it.

In strict placement, the operator releases a scheduling gate only after assigning
ordinal `n` to `placement.zones[n % azCount]`. Required hostname anti-affinity
continues to separate members. The StatefulSet uses OrderedReady creation and
highest-ordinal deletion, so every retained ordinal prefix covers the requested
zones with skew at most one. The operator validates exact owner UID, the complete
ordinal range, actual host zones, and hostname separation before and after a
removal. A wrong-victim or changed-membership observation blocks completion.
Replica effects still use the persisted operation ID and resourceVersion CAS;
lease renewal or generation replacement revokes expiry evidence during recovery.

The ordinal selector replaces topology-spread constraints in strict mode:
combining a single-zone selector with a multi-zone minDomains constraint would
prevent scheduling. Relaxed placement retains the existing soft spread and zone
allowlist, without promising hard distribution after removal.

`bucketWorkload` defaults to `Deployment` for compatibility and is immutable.
Existing storage-reservation hashes retain their prior representation. There is
no automatic Deployment adoption or conversion: changing workload layout on an
existing fleet is rejected. This feature therefore enables strict multi-AZ
contraction for new Ordered fleets; legacy Deployment fleets retain all-victims
admission and may block when a scheduler-independent victim cannot be safe.

## Remaining authority limits

An already admitted Bucket generation can now be superseded by a positively
observed fresh successor in the same Pod UID, host, and IP with a different
container invocation. The pinned runtime performs predecessor recovery before
installing a successor lease (`crates/celld/main.rs`, recovery-before-install),
and lease renewal uses conditional writes (`ownership_store.rs`). The operator
retains both exact generations with a `SupersededBy` chain and requires the live
node record to match the chain's terminal generation. This establishes logical
membership, not physical termination or general-purpose disk fencing. An old
generation's return, unknown replacement, missing terminal record, malformed
chain, peer-log evidence, or unadmitted historical writer blocks progress.

No journal history is discarded, and the journal size guard remains. Bucket garbage
collection before the operator first observes expiry still requires positive
recovery evidence. In pinned celld commit
`12d5b6333fe52717325addcfe1e99e9fd4f77bcd`,
`crates/celld/dead_node_gc.rs::retire_dead_node` performs a conditional tombstone
followed by an unconditional deletion; the source explicitly notes that deletion
can arrive after a successor generation reinstalls the node key. Therefore an
absent object plus elapsed time cannot certify which generation expired. This
implementation does not turn absence into process-fencing or data-recovery proof.

Unit/controller tests cover gate identity, ordinal assignment, legacy hash
compatibility, strict deterministic versus arbitrary victims, replica-CAS crash
replay, wrong-victim rejection, renewed/replaced leases, and metadata collection
after observed expiry. Successor tests cover exact admitted replacement, same-container rejection, unknown predecessors, old-generation revival, missing or expired successors, partial listing, and malformed chains. These do not establish real scheduler or AWS qualification.

Run `make integration-ordered-bucket` for the disposable three-node Kind test
with two simulated AZs, actual gated scheduling, deterministic shrink/grow,
container-generation replacement, manager restart, and acknowledged-write checks.
The harness owns a unique cluster and removes it in `finally`; it never uses the
default kubeconfig. Simulated zone labels do not qualify real EKS failure domains.
