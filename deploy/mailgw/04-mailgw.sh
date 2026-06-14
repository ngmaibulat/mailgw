#!/bin/bash
# Deploy the mailgw container on an edge node and point its logging at the core
# node's logservice. Run the SAME script on each edge node.
#
# Usage:
#   CORE_HOST=10.0.0.10 API_KEY=<shared-secret> bash 04-mailgw.sh
#
# CORE_HOST  hostname/IP of the core node running logservice (port 3000)
# API_KEY    shared secret, must match the core logservice API_KEY
set -euo pipefail

cd "$(dirname "$0")"

: "${CORE_HOST:?Set CORE_HOST to the core node address running logservice on port 3000}"
: "${API_KEY:?Set API_KEY to the shared secret that matches the core logservice}"

# Render the runtime logging config so plugins POST across the network to the
# core node (replaces the co-located 'http://logservice:3000' used in dev).
# routing.json / relays.json / ngmfilter.json are managed per-node separately.
mkdir -p /opt/mailgw/config
cat > /opt/mailgw/config/logging.json <<EOF
{
  "url_delivery": "http://${CORE_HOST}:3000/api/delivery",
  "url_conn": "http://${CORE_HOST}:3000/api/connection",
  "url_queue": "http://${CORE_HOST}:3000/api/queue"
}
EOF
echo "Wrote /opt/mailgw/config/logging.json -> http://${CORE_HOST}:3000"

# API_KEY is interpolated into the compose service so the container's plugins
# attach X-API-Key (mailgw/plugins/functions.js apiHeaders).
export API_KEY

docker compose pull
docker compose up -d

echo "mailgw edge node deployed (CORE_HOST=${CORE_HOST})."
