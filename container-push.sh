#!/bin/bash

pnpm version patch

VER=`pnpm pkg get version`

#docker buildx build --network=host --push -t ngmaibulat/mailgw:v${VER} .

docker buildx build \
    --network=host --push \
    -t ngmaibulat/mailgw:v${VER} \
    -t ngmaibulat/mailgw:latest \
    .
