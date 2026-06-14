#!/bin/bash

# Remove ONLY this project's containers — the compose stack (dev-mailgw-db,
# dev-logservice, dev-mailgw-db-migrator, dev-mailhog) plus the standalone dev
# run (dev-mailgw from container-dev.sh) — never every container on the host.
ids=$(docker ps -aq \
    --filter "name=dev-mailgw" \
    --filter "name=dev-logservice" \
    --filter "name=dev-mailhog")

if [ -n "$ids" ]; then
    docker rm -f $ids
else
    echo "No project containers to remove."
fi
