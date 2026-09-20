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

Release `v0.5.1-ewhauser.1` has a Linux amd64/arm64 image index:

```text
ghcr.io/ewhauser/celld@sha256:78f74de9b5482a428b69f175cd1901b59cc363f3aa398ffd197ade0c9a6a20af
```

The source revision is `f3b7e8c07e6fee53f1752bfb7a30fffbf1d514c8`.
[Release build 35538821096](https://github.com/ewhauser/celld/actions/runs/35538821096)
completed and the published index was verified anonymously. Native binaries and
checksums are attached to the [fork release](https://github.com/ewhauser/celld/releases/tag/v0.5.1-ewhauser.1).
Published and attested artifacts still require the deployment qualification
described above. The operator/launcher image is built and pinned separately.
