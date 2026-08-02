#!/bin/bash
# Verify the host bind-mount dirs exist before starting the container.
set -euo pipefail

for dir in /opt/mailgw/config /opt/mailgw/log /opt/mailgw/queue; do
    if [ ! -d "$dir" ]; then
        echo "Directory $dir not found — create it before deploying."
        exit 1
    fi
done

echo "All required /opt/mailgw dirs present."
