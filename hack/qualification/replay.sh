#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."
go build -o bin/celld-qualify ./cmd/celld-qualify
root=docs/qualification
bin/celld-qualify -before "$root/contraction/before-graceful-metadata.json" -after "$root/contraction/after-graceful-metadata.json" -stopped a
bin/celld-qualify -before "$root/contraction/before-abrupt-metadata.json" -after "$root/contraction/after-abrupt-metadata.json" -stopped a,b
bin/celld-qualify -before "$root/fleet/before-abrupt-metadata.json" -after "$root/fleet/after-abrupt-metadata.json" -stopped a,b
bin/celld-qualify -before "$root/fleet/before-deadline-metadata.json" -after "$root/fleet/after-deadline-metadata.json" -stopped a,b
bin/celld-qualify -before "$root/partition/before-partition-metadata.json" -after "$root/partition/after-partition-metadata.json" -stopped a
expect_block() {
    reason=$1
    shift
    if output=$(bin/celld-qualify "$@" 2>&1); then
        echo 'ERROR: expected conservative block' >&2
        exit 1
    fi
    if [ "$output" != "BLOCKED (offline replay): $reason" ]; then
        printf 'ERROR: unexpected replay failure: %s\n' "$output" >&2
        exit 1
    fi
    printf '%s\n' "$output"
}
expect_block 'session recovery unresolved' -before "$root/contraction/before-graceful-metadata.json" -after "$root/contraction/immediate-graceful-metadata.json" -stopped a
expect_block 'session recovery unresolved' -before "$root/fleet/before-deadline-metadata.json" -after "$root/fleet/immediate-deadline-metadata.json" -stopped a,b
expect_block 'session recovery unresolved' -before "$root/bucket/before-graceful-metadata.json" -after "$root/bucket/after-graceful-metadata.json" -stopped a
expect_block 'unknown node or generation replacement' -before "$root/fleet/before-abrupt-metadata.json" -after "$root/fleet/before-deadline-metadata.json" -stopped a,b
