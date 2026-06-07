#!/bin/bash

#docker build --network=host -t v0.0.24  .

docker buildx build --network=host -t v0.0.24  --load .

