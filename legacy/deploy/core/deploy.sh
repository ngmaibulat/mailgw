#!/bin/bash
# Deploy the core node: db + logservice (+ webui later) via docker compose.
# Run from deploy/core/. Requires deploy/core/.env (see .env.example).
set -euo pipefail

cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "Missing deploy/core/.env — copy .env.example and set real secrets:"
    echo "  cp .env.example .env && \${EDITOR:-vi} .env"
    exit 1
fi

# Install Docker + compose plugin if the compose CLI isn't available yet.
if ! docker compose version >/dev/null 2>&1; then
    bash ../common/install-docker.sh
fi

# db-migrator runs to completion first (compose waits via service_completed_successfully).
docker compose pull
docker compose up -d

echo
echo "Core node up. Smoke-testing logservice auth..."
source .env

# With the key: should be accepted.
curl -fsS -X POST http://localhost:3000/api/connection \
    -H "Content-Type: application/json" \
    -H "X-API-Key: ${API_KEY}" \
    -d @samples/connection/conn.json >/dev/null \
    && echo "  OK: authorized POST accepted"

# Without the key: should be rejected (proves auth is enforced).
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:3000/api/connection \
    -H "Content-Type: application/json" \
    -d @samples/connection/conn.json)
echo "  Unauthorized POST (no key) -> HTTP ${code} (expect 401/403)"
