#!/bin/bash
# Build the logservice-go image locally (--load).
#
# Like mailgw-go's, this does NOT bump the version: mutating the tree as a side
# effect of a build makes the build non-idempotent. Use ./bump.sh.
set -euo pipefail
cd "$(dirname "$0")"

VER=$(tr -d ' \n' < VERSION)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)

docker buildx build \
    --network=host \
    --build-arg VERSION="${VER}" \
    --build-arg COMMIT="${COMMIT}" \
    -t ngmaibulat/logservice-go:v${VER} \
    -t ngmaibulat/logservice-go:latest \
    --load \
    .
