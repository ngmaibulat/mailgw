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

# The webui reads ./certs/server.{key,crt} on boot, and mints a self-signed
# pair there if the directory is empty — so this is a warning, not a stop. A
# self-signed certificate on an internet-facing console authenticates nothing:
# it gets the node up, and it is worth replacing before anybody signs in.
mkdir -p certs
if [ ! -f certs/server.key ] || [ ! -f certs/server.crt ]; then
    echo "NOTE: no TLS pair in deploy/core/certs/ — the console will generate a"
    echo "      self-signed one. Replace it with a real pair and restart:"
    echo "        cp your.key deploy/core/certs/server.key"
    echo "        cp your.crt deploy/core/certs/server.crt"
    echo "        docker compose restart webui"
    echo
fi

# Install Docker + compose plugin if the compose CLI isn't available yet.
if ! docker compose version >/dev/null 2>&1; then
    bash ../common/install-docker.sh
fi

# No separate migrator: logservice applies pending migrations before it binds,
# and the console waits at boot for the tables it reads. On a fresh install that
# means `up -d` returns before either has finished, hence the retries below.
docker compose pull
docker compose up -d

echo
echo "Core node up. Smoke-testing logservice auth (retrying while it migrates)..."
source .env

# With the key: should be accepted. --retry-connrefused because on a fresh
# volume logservice is still migrating when this runs and has not bound yet.
curl -fsS --retry 10 --retry-connrefused --retry-delay 3 \
    -X POST http://localhost:3000/api/connection \
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
# It binds only after the schema it reads exists, so give it the same patience.
webui=$(curl -sk --retry 10 --retry-connrefused --retry-delay 3 \
    -o /dev/null -w '%{http_code}' https://localhost:4000/login || echo 000)
echo "  webui GET /login -> HTTP ${webui} (expect 200)"

echo
echo "Next: create the first admin, then approve each gateway that registers."
# create_user.ts is not copied into the image, so /setup is the way in here. It
# is unauthenticated only until the first admin exists, then redirects to /login.
echo "  https://<this-host>:4000/setup     <- one-time, creates the first admin"
echo "  https://<this-host>:4000/gateways  <- approve each edge node by fingerprint"
