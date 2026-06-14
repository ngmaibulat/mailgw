#!/bin/bash

cd "$(dirname "$0")"

bun pm version patch

VER=$(bun pm pkg get version | tr -d '"')

docker buildx build \
    --network=host --push \
    -t ngmaibulat/logservice:v${VER} \
    -t ngmaibulat/logservice:latest \
    .
