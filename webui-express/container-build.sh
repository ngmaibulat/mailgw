#!/bin/bash

#docker build --network=host -t v0.0.24  .

# cd to the repo root regardless of where this script is invoked from
# (so it works both directly and via the root `pnpm build:webui` script).
cd "$(dirname "$0")/.."

# Bump the webui package version (under webui-express/), then build from the
# repo root context using the relocated Dockerfile (so pnpm-lock.yaml is visible).
( cd webui-express && pnpm version patch )

VER=$(cd webui-express && pnpm pkg get version)

docker buildx build \
    --network=host \
    -f webui-express/Dockerfile \
    -t ngmaibulat/mailgw-webui:v${VER}  \
    -t ngmaibulat/mailgw-webui:latest  \
    --load .
