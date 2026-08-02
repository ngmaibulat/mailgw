FROM node:26-alpine

# Patch OS packages (openssl, etc.) on top of the pinned base so we don't ship
# known-fixed CVEs even when the base tag lags behind Alpine updates.
RUN apk --no-cache -U upgrade

WORKDIR /app

# Build context is the repo root (see legacy/webui-express/container-build.sh) so the
# workspace lockfile is visible. The image keeps a flat /app layout.
COPY legacy/webui-express/package.json /app/package.json
# The root pnpm-lock.yaml is deliberately NOT copied: legacy/webui-express left
# the pnpm workspace when it moved under legacy/, so the root lock no longer
# carries an importer for it. The install below was already standalone and
# unpinned (no --frozen-lockfile).

RUN npm install -g pnpm@11

# Standalone prod install: the workspace lockfile doesn't apply to a single
# package, so we don't use --frozen-lockfile here. None of the deps need native
# builds, so no allowBuilds workaround is required (unlike mailgw).
RUN pnpm install --prod

COPY legacy/webui-express/src       /app/src
COPY legacy/webui-express/db        /app/db
COPY legacy/webui-express/templates /app/templates
COPY legacy/webui-express/public    /app/public

# TLS certs (./certs/server.key + server.crt, read on boot for HTTP/2 via spdy)
# and any .env are supplied at runtime via a volume mount — see
# docker-compose.yaml. The app also needs the DB schema to exist (created by the
# logservice migrations) or it exits on startup.
EXPOSE 4000

CMD ["npm", "run", "start"]
