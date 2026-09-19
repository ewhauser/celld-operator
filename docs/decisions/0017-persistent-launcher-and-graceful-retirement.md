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

Use RWOP for production EBS CSI claims. Retained-volume reactivation preserves
PVC UID, PV UID, CSI volume handle, and a signed disk nonce. The local lock still
provides only same-kernel exclusion. Graceful cross-node reuse instead requires
previously persisted positive termination and follower recovery, same-AZ
placement, exclusive healthy CSI attachment, and a short-lived signed grant to
the exact waiting successor. The launcher atomically persists the new host/boot
before starting celld. Require retained delayed-binding gp2/gp3 EBS classes;
Multi-Attach-capable and unknown storage classes are unsupported. Legacy disks
without captured nonce authority remain restricted to their old host incarnation.
Uncertain process/node deaths remain blocked. No EC2 control, force deletion or
detachment, or administrative completion assertion is introduced.

Graceful retirement stops only the highest ordinal, observes exact stopped
invocation recovery, and waits for survivors to stop depending on the departing
follower. A survivor's advanced ensemble is useful only because the pinned
runtime forces outstanding peer writes through its bucket tiering barrier before
reconfiguration. Check loss declarations after node records and retain disks.
The implementation does not contract below two live nodes: the pin lacks an
external completion watermark for retiring the last follower without an
ensemble replacement.

Journal version 7 retains reads of versions 1–6 and rejects incomplete launcher
and volume authority. Versions 1–5 cannot read the new journal. Once `Stopping`
is durable, the operation retains its identity through termination and recovery;
new desired capacity cannot silently cancel or retarget it. A positive stopped
receipt permits the one cleanup decrement even after the operation deadline,
but pause/loss fences still block that decrement. Timeouts never certify recovery.

See [the implemented protocol, failure boundaries and migration rules](../persistent-fleet-lifecycle.md).

A positive stopped receipt now includes signed `RestartDenied` authority, issued
only after fsyncing a permanent per-Pod-UID startup denial marker. New Pod UIDs
can reuse the disk; any retired UID remains blocked across subsequent cycles.
Markers never recreate positive stop certificates. Legacy journals are readable,
but old retirement receipts without this contract cannot authorize reuse or
resolve historical writers without a separately qualified migration/fencing path.

Amendment (19 September 2026): a `Stopping` operation whose launcher still
reports `Running` after the deadline plus the stop-request expiry margin is
canceled as unissued through the workload-CAS fence. A survivor pod recreated
outside any operation may return onto its unchanged host incarnation and
retained volume, with the old invocation recorded as superseded; the successor
launcher's exclusive lock is the exclusion authority. Changed host or volume
identity remains blocked. See [the lifecycle contract](../persistent-fleet-lifecycle.md).
