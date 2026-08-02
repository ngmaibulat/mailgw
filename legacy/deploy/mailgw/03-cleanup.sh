#!/bin/bash
# Remove any previously running mailgw container before redeploying.
# (pm2/podman are no longer used.)
set -euo pipefail

cd "$(dirname "$0")"

if docker compose version >/dev/null 2>&1 && [ -f docker-compose.yaml ]; then
    docker compose down --remove-orphans 2>/dev/null || true
fi

docker rm -f mailgw 2>/dev/null || true

echo "Cleaned up previous mailgw container."
