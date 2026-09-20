# EKS qualification plan

Run only in an explicitly selected disposable AWS environment. Record the
operator, launcher and runtime image digests, fork commit, node architectures,
Kubernetes and CSI versions, StorageClass, region, zones and IAM roles.

1. Verify private networking and runtime-only bucket credentials. Start a
   homogeneous fork fleet and confirm schema 1 and strict removal capability
   for every member. Verify stable per-Pod peer DNS resolves while Pods are not
   Ready; predecessor recovery must not depend on old Pod IPs.
2. Generate uniquely numbered acknowledged durable writes while scaling an
   Ordered Bucket fleet and a PersistentFleet down and back up. Verify every
   acknowledgment after membership changes and survivor restart.
3. Exercise strict shutdown with follower-only obligations, the last follower,
   delayed S3 operations and transient control-plane outages. Failed or unknown
   completion must preserve the exact current operation and disk.
4. Crash the operator before issuance, after runtime completion, after proof
   capture and after each workload/PVC effect. Reconcile without duplicate or
   stale effects. Replace named resources to verify UID preconditions.
5. Exercise the selected disk policy with real EBS CSI. Verify PVC protection,
   exact PV/handle binding, attachment cleanup and the actual EBS outcome. A
   missing PVC alone does not prove physical volume deletion.
6. Exercise coordinated restart, image change and fleet deletion. Check explicit
   downtime permission, fresh disks, permanent bucket reservation and every
   acknowledged write after recovery.
7. Lose a launcher before proof capture and try unsafe cross-host disk reuse.
   Verify both remain blocked; do not use force detach or finalizer removal.
8. Partition S3 across node-lease expiry after fresh acknowledged writes. Verify
   fail-stop and unchanged disks, then explicit same-host administrative recovery
   with changed Pod IPs and stable peer DNS. Preserve logs before Pod replacement
   and verify every acknowledgment. Test delayed peer startup separately: the
   current runtime can declare bounded loss after its expired-member grace, so a
   simultaneous local restart does not establish this boundary.

Archive commands, timestamps, artifacts and failure results for the exact run.
A local MinIO or Kind pass does not close any unexecuted AWS gate.
