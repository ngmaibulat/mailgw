#!/bin/bash
# Build and push the mailgw-go image.
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
    --push \
    .
