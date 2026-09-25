#!/usr/bin/env bash
set -euo pipefail

# The upstream release images are no longer anonymously pullable. Build the
# disposable Kind test images from the same tagged, checksum-verified Go modules.
server_image=celld-integration/minio:RELEASE.2025-09-07T16-13-09Z
client_image=celld-integration/mc:RELEASE.2025-08-13T08-35-41Z
if docker image inspect "$server_image" "$client_image" >/dev/null 2>&1; then
  exit 0
fi

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
arch="$(go env GOARCH)"
mod_cache="$(go env GOMODCACHE)"

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOPATH="$scratch/go" GOMODCACHE="$mod_cache" go install github.com/minio/minio@RELEASE.2025-09-07T16-13-09Z
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOPATH="$scratch/go" GOMODCACHE="$mod_cache" go install github.com/minio/mc@RELEASE.2025-08-13T08-35-41Z

for binary in minio mc; do
  mkdir -p "$scratch/$binary"
  bin_dir="$scratch/go/bin"
  if [[ -d "$bin_dir/linux_$arch" ]]; then
    bin_dir="$bin_dir/linux_$arch"
  fi
  cp "$bin_dir/$binary" "$scratch/$binary/$binary"
  cat > "$scratch/$binary/Dockerfile" <<EOF
FROM alpine:3.22.2
RUN apk add --no-cache ca-certificates
COPY $binary /usr/local/bin/$binary
ENTRYPOINT ["$binary"]
EOF
done

docker build --platform "linux/$arch" -t "$server_image" "$scratch/minio"
docker build --platform "linux/$arch" -t "$client_image" "$scratch/mc"
