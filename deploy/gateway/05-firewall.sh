#!/bin/bash
# Restrict the mailgw-go admin UI to a management network.
#
# REQUIRED on an edge node, not optional — still, now that the UI has a claim
# code in front of it.
#
# The admin UI is the only way to provision a zero-configuration gateway, so it
# is always listening. It authenticates, but what it protects is the ability to
# re-point this gateway at a Central Manager of somebody's choosing and be handed
# that manager's configuration — and this one's relay credentials on the way.
# Under network_mode: host there is no docker port mapping to narrow, and the
# process runs as root on a machine that accepts mail from the internet. Cutting
# the number of people who can reach an authenticated root process is worth doing
# on its own; authentication turned the firewall from the only control into one
# of two, not into a spare.
#
# Run it once the wizard has been used, or from a management host that this rule
# will still admit. Locking yourself out is the failure mode here.
#
# Usage:
#   MGMT_CIDR=10.0.0.0/24 bash 05-firewall.sh
#   MGMT_CIDR=10.0.0.0/24 ADMIN_PORT=8080 bash 05-firewall.sh
set -euo pipefail

ADMIN_PORT="${ADMIN_PORT:-8080}"

if [ -z "${MGMT_CIDR:-}" ]; then
    echo "MGMT_CIDR is required, e.g. MGMT_CIDR=10.0.0.0/24 bash 05-firewall.sh" >&2
    echo "It is the network allowed to reach the admin UI on port ${ADMIN_PORT}." >&2
    exit 2
fi

if ! command -v ufw >/dev/null 2>&1; then
    echo "ufw is not installed. Add an equivalent rule with your firewall:" >&2
    echo "  allow ${MGMT_CIDR} -> tcp/${ADMIN_PORT}, deny everything else." >&2
    exit 1
fi

# Order matters: ufw evaluates rules top-down and the first match wins, so the
# allow has to be inserted above the deny.
echo "Allowing ${MGMT_CIDR} to reach tcp/${ADMIN_PORT}, denying everyone else."
ufw allow from "${MGMT_CIDR}" to any port "${ADMIN_PORT}" proto tcp
ufw deny "${ADMIN_PORT}/tcp"

echo
echo "Current rules for ${ADMIN_PORT}:"
ufw status | grep -E "(^|\s)${ADMIN_PORT}(/|\s|$)" || true

echo
echo "Note: port 25 is deliberately untouched — this node is a mail relay, and"
echo "the IP allowlist Central Management deploys to it is what gates SMTP."
