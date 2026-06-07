#!/bin/bash

#docker build --network=host -t v0.0.24  .

pnpm version patch

VER=`pnpm pkg get version`

docker buildx build --network=host -t v${VER}  --load .
