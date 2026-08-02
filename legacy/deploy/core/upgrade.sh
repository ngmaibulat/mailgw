#!/bin/bash
# Upgrade the core node to the latest images (re-runs migrations).
# Run from deploy/core/.
set -euo pipefail

cd "$(dirname "$0")"

if [ ! -f .env ]; then
    echo "Missing deploy/core/.env — see .env.example"
    exit 1
fi

docker compose pull
# Re-run schema migrations, then recreate services on the new images.
docker compose run --rm db-migrator
docker compose up -d

echo "Core node upgraded."
