# Runtime requirements

Provisioning requires an explicit registry-pinned OCI image ending in
`@sha256:<64 lowercase hex digits>`. PersistentFleet also requires a
digest-pinned launcher image. There is no default runtime image and no list of stock upstream
releases accepted as substitutes.

The required fork is based on upstream v0.5.1 and exposes strict shutdown schema
1 through the existing `/state` and `/shutdown` control plane. Every possible
recovery participant must understand its native `bucket_complete` proof. See
[the wire contract](runtime-control-plane.md) and [fork source](https://github.com/ewhauser/celld).

Pin validation is syntactic. Before deployment, verify the artifact's source,
platform and digest and verify a homogeneous fleet against the required API and
recovery behavior. A native binary checksum is not a container image digest.
An ECR pull-through cache or other mirror may be used, for example
`123456789012.dkr.ecr.us-east-1.amazonaws.com/cache/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29`.
Confirm that the mirror serves the compatible fork under the pinned digest; the
operator cannot infer image provenance from the reference.

A runtime-image change requests coordinated maintenance, with explicit
`allowCoordinatedDowntime`. The executor strictly stops the current working set
before changing the image and restarting on fresh disks. No rolling upgrade,
old runtime adapter or directional stock-release migration exists. The caller
must establish source/target format compatibility; the operator cannot infer it
from two valid digest strings.

## Published fork artifact

Release `v0.5.1-ewhauser.3` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:4b9eb5656054580e7dd5ed2bbd9ee8b641ecd60c317437e9be63f4e3ae333f29
```

Version `.3` fixes retained-witness startup skew: unreachable witnesses stay
undecided regardless of lease age, and exhausted startup retries fail without
sealing the predecessor merely because a peer is unavailable. Do not use `.2`
for retained-disk recovery. It also includes the `.2` populated full-stop fix:
strict shutdown completes after
durability, runtime stop and ownership release without waiting for successor
adoption. Do not use `.1` for coordinated maintenance.

The source revision is `739f2baa87a5bfc4bfe04e317adf6d774edf8740`.
[Release build 35547238694](https://github.com/ewhauser/celld/actions/runs/35547238694)
and [image build 35548054541](https://github.com/ewhauser/celld/actions/runs/35548054541)
completed; the published index was verified anonymously. Native binaries and
checksums are attached to the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.3).
Confirm deployment behavior with the exact artifact and storage configuration you use. The operator/launcher image is built and pinned separately.
