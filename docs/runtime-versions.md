# Runtime requirements

Provisioning requires an explicit registry-pinned OCI image ending in
`@sha256:<64 lowercase hex digits>`. There is no default runtime image and no
list of stock upstream releases accepted as substitutes.

The required fork is based on upstream v0.5.1 and exposes its state through the
existing `/state` control plane, including `node_log` from `.5`. Every possible
recovery participant must understand its native `bucket_complete` proof. See
[the wire contract](runtime-control-plane.md) and [fork source](https://github.com/ewhauser/celld).

Pin validation is syntactic. Before deployment, verify the artifact's source,
platform and digest and verify a homogeneous fleet against the required API and
recovery behavior. A native binary checksum is not a container image digest.
An ECR pull-through cache or other mirror may be used, for example
`123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29`.
Confirm that the mirror serves the compatible fork under the pinned digest; the
operator cannot infer image provenance from the reference.

A runtime-image change is a rolling update for both profiles. PersistentFleet
replaces one member at a time on its retained disk, each only once the fleet has
settled, so old and new versions run together during the rollout. No old runtime
adapter or directional stock-release migration exists. The caller must establish
source/target format compatibility; the operator cannot infer it from two valid
digest strings.

PersistentFleet settlement and disk release read celld's `/state.node_log`,
first published in `0.5.1-ewhauser.5`. Earlier runtimes still roll restarts and
upgrades on readiness plus a one-minute stabilization, so an older fleet can
upgrade in place. They never authorize disk deletion. On `.5`, an idle leader
never lets go of a removed follower, so a removed member's disk is never
released; scale-in of a PersistentFleet requires `.6` or later on every member.

## Published fork artifact

Release `v0.5.1-ewhauser.7` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:c6b28dd2cc7b80ac910013df06951dc1a06409cab3f98185594fe0f246d6039e
```

`.6` adds to `.5`'s node-log state: three failed idle probes to a follower
degrade the leader's ensemble exactly as a failed write does, so an idle fleet
moves off a departed member and releases its obligations. `.7` lets a
replacement disk under a member's own name answer recovery conclusively, so a
fleet that loses every copy of a session (for example both members of a
two-member fleet at once) records a bounded loss and recovers instead of
waiting forever.

The source revision is `9413a0bafd596db273649ed470ca8a1527263aec`.
[Release build 36213756956](https://github.com/ewhauser/celld/actions/runs/36213756956)
and [image build 36214420376](https://github.com/ewhauser/celld/actions/runs/36214420376)
completed. The binary checksums, build provenance, `celld --version` and the
image index's build attestation were verified. Native binaries and checksums are
attached to the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.7).
Confirm deployment behavior with the exact artifact and storage configuration you use. The operator image is built and pinned separately.
