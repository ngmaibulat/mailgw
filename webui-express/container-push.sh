#!/bin/bash

# cd to the repo root regardless of where this script is invoked from
# (so it works both directly and via the root `pnpm build:webui` script).
cd "$(dirname "$0")/.."

# Bump the webui package version (under webui-express/), then build + push from
# the repo root context using the relocated Dockerfile.
( cd webui-express && pnpm version patch )

VER=$(cd webui-express && pnpm pkg get version)

docker buildx build \
    --network=host --push \
    -f webui-express/Dockerfile \
    -t ngmaibulat/mailgw-webui:v${VER} \
    -t ngmaibulat/mailgw-webui:latest \
    .
