#!/bin/bash
# Deploy the core node: mariadb + logservice + webui via docker compose.
# Run from deploy/core/. Requires deploy/core/.env (see .env.example) and a TLS
# cert pair in deploy/core/certs/ (the webui will not boot without one).
set -euo pipefail

cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "Missing deploy/core/.env — copy .env.example and set real secrets:"
    echo "  cp .env.example .env && \${EDITOR:-vi} .env"
    exit 1
fi

# The webui reads ./certs/server.{key,crt} on boot and crashes without them.
if [ ! -f certs/server.key ] || [ ! -f certs/server.crt ]; then
    echo "Missing deploy/core/certs/server.{key,crt} — the webui needs a TLS pair."
    echo "  For a self-signed pair, from the repo root:"
    echo "    pnpm certs && cp certs/generated/webui/server.{key,crt} deploy/core/certs/"
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

# The webui serves HTTP/2 over TLS with a self-signed cert by default, hence -k.
webui=$(curl -sk -o /dev/null -w '%{http_code}' https://localhost:4000/login || echo 000)
echo "  webui GET /login -> HTTP ${webui} (expect 200)"

echo
echo "Next: create the first admin, then approve each gateway that registers."
# create_user.ts is not copied into the image, so /setup is the way in here. It
# is unauthenticated only until the first admin exists, then redirects to /login.
echo "  https://<this-host>:4000/setup     <- one-time, creates the first admin"
echo "  https://<this-host>:4000/gateways  <- approve each edge node by fingerprint"
