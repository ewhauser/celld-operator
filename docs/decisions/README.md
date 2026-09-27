# Architecture decisions

[0023: Node loss is routine](0023-node-loss-is-routine.md). celld tolerates
losing one node, so the operator proves no removals and never waits on a disk
that is gone. Both profiles run celld directly with no launcher. Bucket fleets
are plain rolling workloads, and voluntary disruption is one member at a time.

[0024: PersistentFleet is a StatefulSet](0024-persistentfleet-is-a-statefulset.md).
The StatefulSet's rolling update restarts members and celld recovers them.
Every member keeps its disk until the fleet is deleted or the member cannot
come back, the operator replaces such a member itself, and it keeps no
lifecycle state.

Superseded records, including 0022's strict removal proof, and their evidence
are in Git history. The operator has no compatibility path for fleets created
by those implementations beyond the in-place adoption described in 0024.
