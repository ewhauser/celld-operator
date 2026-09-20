# Runtime requirements

Provisioning requires an explicit image matching
`ghcr.io/ewhauser/celld@sha256:<64 lowercase hex digits>` and a digest-pinned
launcher image. There is no default runtime image and no list of stock upstream
releases accepted as substitutes.

The required fork is based on upstream v0.5.1 and exposes strict shutdown schema
1 through the existing `/state` and `/shutdown` control plane. Every possible
recovery participant must understand its native `bucket_complete` proof. See
[the wire contract](runtime-control-plane.md) and [fork source](https://github.com/ewhauser/celld).

Pin validation is syntactic. Before deployment, verify the artifact's source,
platform and digest and qualify a homogeneous fleet against the required API and
recovery behavior. A native binary checksum is not a container image digest.

A runtime-image change requests coordinated maintenance, with explicit
`allowCoordinatedDowntime`. The executor strictly stops the current working set
before changing the image and restarting on fresh disks. No rolling upgrade,
old runtime adapter or directional stock-release migration exists. The caller
must establish source/target format compatibility; the operator cannot infer it
from two valid digest strings.

## Published fork artifact

Release `v0.5.1-ewhauser.2` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:a00da2bcaeaee6879d658477cd1bdb354a5de55fa9e7f0ab5e2fd95e6e0ce080
```

Version `.2` fixes populated full-stop removal: strict shutdown completes after
durability, runtime stop and ownership release without waiting for successor
adoption. Do not use `.1` for coordinated maintenance.

The source revision is `2a65a4df99bed5254bdd679df57ed98556454dec`.
[Release build 35542493819](https://github.com/ewhauser/celld/actions/runs/35542493819)
completed and the published index was verified anonymously. Native binaries and
checksums are attached to the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.2).
Published and attested artifacts still require the deployment qualification
described above. The operator/launcher image is built and pinned separately.
