# Local preview integration

`make integration-previews` exercises the real operator and celld CLI in an
isolated, single-node Kind cluster. It installs Calico, shared MinIO, Traefik,
and cluster-local DNS for `*.previews.test`. It uses separate developer and
seed-executor ServiceAccounts and never fabricates controller status, seed
receipts, or startup gates.

## Run

Requirements: Go, Docker, Kind, kubectl, network access for pinned dependencies,
and a celld checkout containing **both** the snapshot runtime and preview CLI
changes. A runtime-only checkout is insufficient. Allow several GB of free space
on the host and Docker VM, and at least 4 CPUs / 8 GB for Docker.

```sh
# Build from your celld checkout. Optional persistent host build cache:
export CELLD_PREVIEW_TARGET_DIR="$HOME/.cache/celld-preview-target"
./hack/build-preview-runtime.sh /absolute/path/to/celld
CELLD_PREVIEW_IMAGE=localhost/celld-preview:integration make integration-previews
```

The build script creates a local development image and validates `celld preview
--help`. The suite imports the actual OCI manifest digest into Kind and uses
that immutable reference in fleet specs. Nothing is pushed to a registry.
Alternatively, supply a published fork image containing both features:

```sh
make integration-previews RUNTIME_IMAGE=ghcr.io/ewhauser/celld@sha256:YOUR_DIGEST
```

The ordinary runtime pin may predate preview CLI support. No existing cluster,
default kubeconfig, host DNS configuration, or production store is used. The
suite removes only its uniquely named cluster and launcher image on success,
failure, or interruption. The caller's runtime image and optional build cache
remain available for reruns. Logs go to the terminal, not the repository.

The preview suite is opt-in: it is not part of `make integration` or the current
CI matrix. Run it explicitly with an image containing both preview features.

## Coverage

- Create previews through the CLI; resolve and serve distinct stable URLs through
  real DNS, ingress, NetworkPolicy, and runtime resources.
- Copy two Durable Objects, including SQL and KV data; check both `Clear` and
  `Preserve` alarm policies and source/preview state isolation.
- Block runtime and route creation before seed completion; execute one request
  directly and another through `celld preview seed --watch`.
- Redeploy code, wait for observed application convergence, and preserve URL,
  object identity, and state.
- Expire a running preview and explicitly delete others through normal fleet
  shutdown; retain storage reservations.
- Cancel an unclaimed seed and a running seed. Interrupt an executor, restart it
  and the operator, and verify that a Running claim cannot be stolen or silently
  released. The ambiguous claim remains fenced until the disposable cluster is
  destroyed.

Source fixtures are themselves previews so their persisted state uses the real
custom-endpoint contract. The harness waits for remote LTX checkpoints before
cloning; HTTP acknowledgement alone is not a snapshot boundary.

This is HTTP qualification of a local disposable store. It does not qualify
public DNS, TLS certificates, AWS credentials, S3 durability, or production
capacity. See [preview setup](previews.md) for those deployment requirements.

For a bounded diagnostic hold after failure:

```sh
CELLD_TEST_DIAGNOSTIC_HOLD_SECONDS=300 \
  CELLD_PREVIEW_IMAGE=localhost/celld-preview:integration make integration-previews
```

The harness prints the private kubeconfig and exact cluster name. Interrupting
it still triggers cleanup. Never clear retained seed claims or finalizers to
make a failed test pass.
