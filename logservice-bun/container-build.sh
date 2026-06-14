#!/bin/bash

#docker build --network=host -t v0.0.24  .

bun pm version patch

VER=$(bun pm pkg get version | tr -d '"')

docker buildx build \
    --network=host \
    -t ngmaibulat/logservice:v${VER}  \
    -t ngmaibulat/logservice:latest  \
    --load .
