#!/bin/bash

# cd to the repo root regardless of where this script is invoked from
# (so it works both directly and via a root pnpm script).
cd "$(dirname "$0")/.."

# Bump the webui-fastify package version, then build + push from the repo root
# context using the relocated Dockerfile.
( cd webui-fastify && pnpm version patch )

VER=$(cd webui-fastify && pnpm pkg get version)

docker buildx build \
    --network=host --push \
    -f webui-fastify/Dockerfile \
    -t ngmaibulat/mailgw-webui-fastify:v${VER} \
    -t ngmaibulat/mailgw-webui-fastify:latest \
    .
