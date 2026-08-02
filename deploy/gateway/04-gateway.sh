#!/bin/bash
# Bring up the mailgw-go container on an edge node. Run the SAME script on each.
#
# There is nothing to configure here, and that is the point. The node is
# zero-configuration: it boots with no arguments, no environment and no config
# files, generates its own Ed25519 identity, and takes everything it runs on —
# rule set, IP allowlist, relay table, logservice endpoints and API key — from
# Central Management, cached locally so a console outage is a non-event.
#
# Usage:
#   bash 04-gateway.sh
set -euo pipefail

cd "$(dirname "$0")"

docker compose pull
docker compose up -d

# The wizard binds every interface, so print an address an operator can actually
# reach rather than "0.0.0.0".
host_addr=$(hostname -I 2>/dev/null | awk '{print $1}')
host_addr=${host_addr:-<this-node>}

# The claim code that unlocks the wizard. Read it BEFORE the heredoc below: that
# heredoc is unquoted so ${host_addr} expands, which means a $(...) inside it
# would run mid-render and put docker's own output in the middle of the
# instructions.
#
# `docker compose up -d` returns as soon as the container is created, which is
# before the process has opened its database, so this retries rather than
# assuming.
claim_code=""
for _ in 1 2 3 4 5; do
    claim_code=$(docker compose exec -T mailgw-go \
        /usr/local/bin/mailgw-go claim status 2>/dev/null | tail -n1 || true)
    [ -n "$claim_code" ] && break
    sleep 1
done
if [ -n "$claim_code" ]; then
    claim_line="Claim code: ${claim_code}"
else
    claim_line="Claim code: run  docker exec mailgw-go /usr/local/bin/mailgw-go claim status"
fi

cat <<EOF

mailgw-go is running, and is deliberately NOT relaying mail yet. Finish it:

  1. Open the local wizard:  http://${host_addr}:8080
     ${claim_line}
     Paste the code to sign in. Then enter your Central Management URL — if the
     console serves a self-signed certificate (the shipped default), tick the
     trust checkbox — and press Register. The page then shows this gateway's
     fingerprint.

  2. In the console, open /gateways, find the matching fingerprint, approve it.

  3. Assign a rule set, an allowlist and a relay group, then press Deploy. The
     gateway applies it within a second and starts listening on port 25.

Afterwards:

  docker logs -f mailgw-go
  docker exec mailgw-go /usr/local/bin/mailgw-go config show   # what it is running
  docker exec mailgw-go /usr/local/bin/mailgw-go check         # is that valid?
  docker exec mailgw-go /usr/local/bin/mailgw-go claim reset   # lost the code

SECURITY: port 8080 is this node's admin UI, served by a root process on an
internet-facing relay. It is authenticated by the claim code above, but whoever
signs in can re-point this gateway at another Central Manager and be handed its
relay credentials — so narrow what can reach it as well:

  MGMT_CIDR=<your management network> bash 05-firewall.sh

BACKUP: /opt/mailgw-go/data holds this gateway's private key, its claim code and
its cached configuration. Lose it and the node re-registers as a stranger, and
stops relaying until an operator approves it again.
EOF
