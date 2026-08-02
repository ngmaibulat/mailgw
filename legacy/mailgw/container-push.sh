#!/bin/bash

# cd to the repo root regardless of where this script is invoked from
# (so it works both directly and via a root pnpm script).
cd "$(dirname "$0")/../.."

# Bump the Haraka package version (under legacy/mailgw/), then build + push from the
# repo root context using the relocated Dockerfile.
( cd legacy/mailgw && pnpm version patch --no-git-tag-version )

VER=$(cd legacy/mailgw && pnpm pkg get version)

docker buildx build \
    --network=host --push \
    -f legacy/mailgw/Dockerfile \
    -t ngmaibulat/mailgw:v${VER} \
    -t ngmaibulat/mailgw:latest \
    .
