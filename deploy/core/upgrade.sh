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
# Apply the new image's migrations BEFORE recreating anything.
#
# logservice migrates on start too, so this is not needed for correctness — it
# is needed for the failure case. Run this way, a bad migration aborts the
# script here (set -e) with the old stack still serving. Skip it, and the same
# migration takes logservice down into a restart loop, and the console with it.
#
# `run` starts mariadb via depends_on and publishes no ports, so it cannot
# collide with the logservice already listening on 3000.
docker compose run --rm logservice migrate
docker compose up -d

echo "Core node upgraded."
