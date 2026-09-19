# ADR 0017 PersistentFleet launcher and graceful retirement

Status: Accepted direction; implemented experimental graceful path, cloud qualification pending

Date: 18 September 2026

The user approved an operator-owned launcher around the unchanged pinned celld
binary. New PersistentFleet deployments may opt into this path with a
digest-pinned `--launcher-image`. Bucket's runtime template is unchanged.

The launcher holds a volume lock and passes its descriptor into celld. A live,
authenticated response from the original launcher can certify its exact child
has exited and all inherited lock holders have released access. File markers,
pod disappearance and lease expiry are not termination certificates. Runtime
generation is bound using the existing reexec probe-signing-key handoff, without
reading the fleet peer secret or adding S3 permissions.

Use RWOP for production EBS CSI claims. Restrict retained-volume reactivation to
the same Kubernetes Node UID, boot ID, PVC UID, PV UID and CSI volume handle.
A scheduling gate pins reactivation to that host; the launcher independently
refuses a changed host name or kernel boot ID. Cross-host reuse and uncertain
process/node failures remain blocked. No EC2 control, force deletion/detachment,
or administrative completion assertion is introduced.

Graceful retirement stops only the highest ordinal, observes exact stopped
invocation recovery, and waits for survivors to stop depending on the departing
follower. A survivor's advanced ensemble is useful only because the pinned
runtime forces outstanding peer writes through its bucket tiering barrier before
reconfiguration. Check loss declarations after node records and retain disks.
The implementation does not contract below two live nodes: the pin lacks an
external completion watermark for retiring the last follower without an
ensemble replacement.

Journal version 6 retains reads of versions 1–5 and rejects incomplete launcher
and volume authority. Versions 1–5 cannot read the new journal. Once `Stopping`
is durable, the operation retains its identity through termination and recovery;
new desired capacity cannot silently cancel or retarget it. A positive stopped
receipt permits the one cleanup decrement even after the operation deadline,
but pause/loss fences still block that decrement. Timeouts never certify recovery.

See [the implemented protocol, failure boundaries and migration rules](../persistent-fleet-lifecycle.md).
