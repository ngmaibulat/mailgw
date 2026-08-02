#!/bin/bash
# Remove any previously running gateway container before redeploying.
set -euo pipefail

cd "$(dirname "$0")"

if docker compose version >/dev/null 2>&1 && [ -f docker-compose.yaml ]; then
    docker compose down --remove-orphans 2>/dev/null || true
fi

# Both names: `mailgw` is the legacy Haraka container this node may still run.
docker rm -f mailgw-go 2>/dev/null || true
docker rm -f mailgw 2>/dev/null || true

echo "Cleaned up previous gateway container."
