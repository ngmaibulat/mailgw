#!/bin/bash
# Verify the host bind-mount dirs exist before starting the container.
# mailgw-go logs to stdout (docker logs), so unlike Haraka there is no log dir.
set -euo pipefail

# This node has no config directory: the rule set, the allowlist, the relay
# table and the logservice endpoints all arrive from Central Management. Only the
# two stateful directories are needed.
#
# The queue is mounted at /var/lib/mailgw-go/queue inside the container, beside
# the store rather than at /opt — that is where a gateway spools when the
# deployed server profile does not name a directory, which is the normal case.
if [ ! -d /opt/mailgw-go/queue ]; then
    echo "Creating /opt/mailgw-go/queue (outbound spool)."
    mkdir -p /opt/mailgw-go/queue
fi

if [ -d /opt/mailgw-go/config ]; then
    echo "Note: /opt/mailgw-go/config exists but is no longer mounted."
    echo "  This node is centrally managed; its configuration comes from the console."
    echo "  Keep the directory for reference or remove it — nothing reads it."
fi

# The container runs as root here (see docker-compose.yaml), so it can write the
# spool regardless of owner — but a queue dir that is not a directory, or lives
# on a different filesystem from its own subdirs, breaks the rename(2) contract
# the spool relies on for crash safety.
if [ ! -w /opt/mailgw-go/queue ]; then
    echo "/opt/mailgw-go/queue is not writable — the outbound spool needs it."
    exit 1
fi

# The SQLite store holds this gateway's Ed25519 private key AND its cached
# configuration, so it is created 0700 rather than left to the umask. Losing it
# means the node re-registers as a stranger and stops relaying until an operator
# approves it again — back this directory up.
if [ ! -d /opt/mailgw-go/data ]; then
    echo "Creating /opt/mailgw-go/data (gateway identity store)."
    mkdir -p /opt/mailgw-go/data
fi
chmod 0700 /opt/mailgw-go/data

mode=$(stat -c '%a' /opt/mailgw-go/data)
if [ "$mode" != "700" ]; then
    echo "/opt/mailgw-go/data is mode $mode — it holds a private key and must be 0700."
    exit 1
fi

echo "All required /opt/mailgw-go dirs present."
