#!/bin/bash
# Build the ENGINEERING image locally (--load).
#
# This image carries an unauthenticated HTTP control API for configuring a
# gateway from a test — see internal/testctl/doc.go. It must never be deployed.
#
# Three things here are deliberate and load-bearing:
#
#   --target test   the shipped stage is the LAST one in the Dockerfile, so a
#                   build without this produces the production image. Reaching
#                   the engineering one has to be explicit.
#   a different repository name, so no compose file that pulls
#                   ngmaibulat/mailgw-go can ever receive this by accident.
#   NO :latest tag, for the same reason — :latest is what a stale compose file
#                   resolves to.
#
# There is deliberately no container-push-test.sh. If this image ever needs to
# leave the machine that built it, that is a decision worth making on purpose.
set -euo pipefail
cd "$(dirname "$0")"

VER=$(tr -d ' \n' < VERSION)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)

docker buildx build \
    --network=host \
    --target test \
    --build-arg VERSION="${VER}" \
    --build-arg COMMIT="${COMMIT}" \
    -t ngmaibulat/mailgw-go-test:v${VER} \
    --load \
    .

cat <<EOF

Built ngmaibulat/mailgw-go-test:v${VER}

  This image serves an UNAUTHENTICATED configuration API. Run it only on a
  network you control, and never as a production gateway.
EOF
