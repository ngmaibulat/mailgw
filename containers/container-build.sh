#!/bin/bash

#docker build --network=host -t v0.0.24  .

cd ..

pnpm version patch

VER=`pnpm pkg get version`

docker buildx build \
    --network=host \
    -t ngmaibulat/mailgw:v${VER}  \
    -t ngmaibulat/mailgw:latest  \
    --load .
