#!/usr/bin/env bash
# Build a local integration image from a celld checkout containing BOTH the
# preview snapshot runtime and CLI/executor changes. This is not a release build.
set -euo pipefail
source_dir=$(cd "${1:?usage: hack/build-preview-runtime.sh CELLD_CHECKOUT [IMAGE]}" && pwd)
image=${2:-localhost/celld-preview:integration}
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/celld-preview-build.XXXXXX")
trap 'rm -rf "$build_dir"' EXIT
# Put the large compilation output on the host, not inside Docker's VM disk.
# Set CELLD_PREVIEW_TARGET_DIR to retain it across builds.
target_dir=${CELLD_PREVIEW_TARGET_DIR:-$build_dir/target}
mkdir -p "$target_dir"
target_dir=$(cd "$target_dir" && pwd)
docker run --rm \
  --mount "type=bind,src=$source_dir,dst=/src,readonly" \
  --mount "type=bind,src=$target_dir,dst=/target" \
  --mount type=volume,src=celld-preview-cargo-registry,dst=/usr/local/cargo/registry \
  -w /src -e CARGO_TARGET_DIR=/target -e CARGO_PROFILE_DEV_DEBUG=0 \
  -e CARGO_PROFILE_DEV_INCREMENTAL=false -e CARGO_BUILD_JOBS=1 \
  rust:1.97.1-bookworm cargo build --locked -p celld --bin celld
mkdir "$build_dir/image"
cp "$target_dir/debug/celld" "$build_dir/image/celld"
docker build --load -t "$image" -f "$repo_dir/hack/integration/preview-runtime.Dockerfile" "$build_dir/image"
docker run --rm "$image" preview --help
printf 'Run: CELLD_PREVIEW_IMAGE=%q make integration-previews\n' "$image"
