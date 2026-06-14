# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a mail gateway/router built on [Haraka](https://haraka.github.io/) (Node.js SMTP server). It accepts inbound SMTP, applies routing rules to forward mail to configured relay targets, and POSTs structured JSON events to a companion logging service.

The repo is a pnpm workspace monorepo. Each app lives in its own top-level
package under a private workspace root (`package.json` + `pnpm-workspace.yaml`):
- **`mailgw/`** — the Haraka SMTP server with custom plugins (`mailgw/plugins/`)
- **`logservice/`** — an Express + Sequelize REST API that receives events from plugins and stores them in MariaDB

## Commands

### Haraka (`mailgw/` package)

Run via the workspace filter from the repo root, or `cd mailgw` first:

```bash
pnpm --filter @aibulat/mailgw start   # run Haraka on port 25  (or: cd mailgw && pnpm start)
pnpm --filter @aibulat/mailgw dev     # run with MODE=DEV (extra debug logging to log/)
pnpm --filter @aibulat/mailgw test    # run the plugin test suite
cd mailgw && pnpm run client          # send a test email via swaks to localhost
cd mailgw && pnpm run plugin-cfg      # dump Haraka plugin configuration
cd mailgw && pnpm version patch       # bump patch version (used before a container build)
```

### logservice

```bash
cd logservice
pnpm dev                    # run with nodemon (auto-reload)
pnpm start                  # run node src/index.js directly
```

### Container / Docker

```bash
./containers/container-build.sh   # auto-bumps mailgw version, builds with docker buildx (-f mailgw/Dockerfile, root context)
./containers/container-push.sh    # push to Docker Hub (ngmaibulat/mailgw)
cd mailgw && ./container-dev.sh   # run latest image locally (mounts mailgw/plugins live)
docker compose up                 # full stack: mailgw + mariadb + db-migrator
docker compose run --rm db-migrator  # run Sequelize migrations against MariaDB
```

## Architecture

### Haraka plugin pipeline

Haraka loads plugins listed in `mailgw/config/plugins` and calls registered hooks at each SMTP stage. All custom plugins live in `mailgw/plugins/` and follow the `np` naming prefix:

| Plugin | Hook(s) | Purpose |
|---|---|---|
| `npRoute.js` | `hook_get_mx`, `hook_connect` | Route outbound mail via `RoutingTable` |
| `npFilter.js` | `hook_connect`, `hook_rcpt` | IP allowlist enforcement |
| `npFilterAttach.js` | — | Attachment checking |
| `npConnection.js` | `hook_connect` | Log connection events → logservice |
| `npData.js` | `hook_data` | Log DATA-stage events → logservice |
| `npQueue.js` | `hook_queue_outbound` | Log queue events → logservice |
| `npLogDelivery.js` | `hook_delivered` | Log delivery outcomes → logservice |

All logging plugins read `mailgw/config/logging.json` (via Haraka's `this.config.get`) and use `mailgw/plugins/functions.js#httplog` to POST JSON to the logservice.

### Routing logic (`mailgw/plugins/Route.js`, `mailgw/plugins/RoutingTable.js`)

On `register`, `npRoute.js` loads two config files and builds a `RoutingTable`:
- `mailgw/config/routing.json` — array of route rules, each specifying `relay`, `sender`, `sender_domain`, `rcpt`, `rcpt_domain` (empty string = wildcard)
- `mailgw/config/relays.json` — map of relay name → relay object (host, port, etc.)

On `hook_get_mx`, `RoutingTable.findRoute(sender, rcpt)` walks the rules in order and returns the first matching relay.

### Runtime config files (not in repo, mounted at `/opt/mailgw/config`)

- `connection.ini` — required Haraka connection settings
- `routing.json` — route rules
- `relays.json` — relay definitions
- `logging.json` — logservice endpoint URLs (`url_conn`, `url_queue`, `url_delivery`)
- `ngmfilter.json` — IP allowlist `{ "allowed": ["127.0.0.1", ...] }`

### logservice

Express app (`logservice/src/index.js`) listening on `PORT` (default 3000):
- `POST /api/connection` — inbound connection events
- `POST /api/queue` — queue events
- `POST /api/delivery` — delivery events (validated with Zod schema)

Sequelize models in `logservice/models/` backed by MariaDB. Schema migrations live in `logservice/migrations/` and are run via the `db-migrator` container.

### DEV mode

Setting `MODE=DEV` (`pnpm dev`) enables extra JSON logging to `log/ngmroute.log` inside `npRoute.js`. Log files are written to `log/` relative to the Haraka working directory (`mailgw/` locally or `/opt/mailgw` in Docker).
