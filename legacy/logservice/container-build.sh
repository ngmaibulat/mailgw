#!/bin/bash

#docker build --network=host -t v0.0.24  .

# cd to the logservice package dir regardless of invocation cwd (so it works
# both directly and via the root `pnpm build:logservice` script).
cd "$(dirname "$0")"

bun pm version patch

VER=$(bun pm pkg get version | tr -d '"')

docker buildx build \
    --network=host \
    -t ngmaibulat/logservice:v${VER}  \
    -t ngmaibulat/logservice:latest  \
    --load .
