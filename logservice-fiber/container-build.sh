#!/bin/bash
# Build the logservice-fiber image locally (--load).
#
# Note the final `..`: the build context is the REPO ROOT, not this directory,
# because go.mod replaces logservice-go with the sibling directory and Go reads
# a replacement's go.mod during module resolution. See the Dockerfile.
#
# Like logservice-go's, this does NOT bump the version: mutating the tree as a
# side effect of a build makes the build non-idempotent. Use ./bump.sh.
set -euo pipefail
cd "$(dirname "$0")"

VER=$(tr -d ' \n' < VERSION)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)

docker buildx build \
    --network=host \
    --build-arg VERSION="${VER}" \
    --build-arg COMMIT="${COMMIT}" \
    -f Dockerfile \
    -t ngmaibulat/logservice-fiber:v${VER} \
    -t ngmaibulat/logservice-fiber:latest \
    --load \
    ..
