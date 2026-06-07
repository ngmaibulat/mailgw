#!/bin/bash

VER=`pnpm pkg get version`

docker buildx build --network=host --push -t ngmaibulat/mailgw:v${VER} .
