#!/bin/bash
# Build and push the logservice-fiber image. Repo-root context; see the
# Dockerfile and container-build.sh.
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
    --push \
    ..
