#!/bin/bash
# Build the mailgw-go image locally (--load).
#
# Unlike the Node scripts, this does NOT bump the version: mutating the tree as
# a side effect of a build makes the build non-idempotent. Use ./bump.sh.
set -euo pipefail
cd "$(dirname "$0")"

VER=$(tr -d ' \n' < VERSION)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)

docker buildx build \
    --network=host \
    --build-arg VERSION="${VER}" \
    --build-arg COMMIT="${COMMIT}" \
    -t ngmaibulat/mailgw-go:v${VER} \
    -t ngmaibulat/mailgw-go:latest \
    --load \
    .
