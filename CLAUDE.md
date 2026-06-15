# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a mail gateway/router built on [Haraka](https://haraka.github.io/) (Node.js SMTP server). It accepts inbound SMTP, applies routing rules to forward mail to configured relay targets, and POSTs structured JSON events to a companion logging service.

The repo is a monorepo with a private root `package.json`. Only `mailgw/` is a
pnpm workspace member (see `pnpm-workspace.yaml`); `logservice/` and `tests/`
are standalone **Bun** packages with their own `bun.lock`, deliberately kept out
of the pnpm workspace so pnpm and Bun don't fight over `node_modules`.
- **`mailgw/`** — the Haraka SMTP server with custom plugins (`mailgw/plugins/`); a Node.js / pnpm package
- **`logservice/`** — a Bun HTTP API (`Bun.serve`) that receives events from the plugins and stores them in MariaDB via Bun's native SQL client (`Bun.SQL`, MySQL adapter); written in TypeScript
- **`tests/`** — cross-cutting end-to-end tests (Bun): logservice API (`tests/api/`) and SMTP pipeline (`tests/smtp/`)

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
bun run dev                 # run with bun --watch (auto-reload)  → src/index.ts
bun run start               # run src/index.ts directly
bun run start:migrate       # run migrations on boot, then start the server
bun run db:migrate          # apply pending SQL migrations and exit
bun run db:reset            # drop everything, then re-migrate
bun test tests/             # run the unit test suite
```

### Container / Docker

```bash
./mailgw/container-build.sh       # auto-bumps mailgw version, builds with docker buildx (-f mailgw/Dockerfile, root context)
./mailgw/container-push.sh        # build + push to Docker Hub (ngmaibulat/mailgw)
cd mailgw && ./container-dev.sh   # run latest image locally (mounts mailgw/plugins live)
docker compose up                 # full stack: mailgw + mariadb + db-migrator
docker compose run --rm db-migrator  # apply SQL migrations against MariaDB (runs `bun src/dbmigrate.ts`)
```

### End-to-end tests (`tests/` package)

`tests/` is a standalone Bun package (not a pnpm workspace member) holding
cross-cutting e2e tests that talk to a **running** stack (`docker compose up -d`):
- `tests/api/` — logservice HTTP API e2e (`logservice.e2e.test.ts`)
- `tests/smtp/` — Bun-native SMTP client + pipeline e2e (moved from `client/`)

Run from the repo root so Bun auto-loads the root `.env` (`PORT`, `DB_*`):

```bash
pnpm test:e2e          # all e2e (or: bun test tests/)
pnpm test:e2e:api      # logservice API only (bun test tests/api)
pnpm test:e2e:smtp     # SMTP only (bun test tests/smtp)
```

DB-mutating suites are opt-in: `MAILGW_API_E2E=1` (api) and `MAILGW_DB_CHECK=1`
(smtp). See `tests/README.md`.

## Architecture

### Haraka plugin pipeline

Haraka loads plugins listed in `mailgw/config/plugins` and calls registered hooks at each SMTP stage. All custom plugins live in `mailgw/plugins/` and follow the `np` naming prefix:

| Plugin | Hook(s) | Purpose |
|---|---|---|
| `npRoute.js` | `hook_get_mx` | Route outbound mail via `RoutingTable` |
| `npFilter.js` | `hook_connect`, `hook_rcpt`, `hook_queue_outbound` | IP allowlist enforcement (plus local rcpt logging) |
| `npConnection.js` | `hook_connect` | Write connection info to a local log file + IP-blacklist placeholder (does **not** POST to logservice) |
| `npData.js` | `hook_data` | POST connection info → logservice (`url_conn`) |
| `npQueue.js` | `hook_queue_outbound` | POST transaction/queue event → logservice (`url_queue`) |
| `npLogDelivery.js` | `hook_delivered` | POST delivery outcome → logservice (`url_delivery`) |
| `npFilterAttach.js` | `hook_data_post` | Attachment MD5 blocklist check via POST `/filter/md5`; also POSTs connection + transaction events |

The logservice-posting plugins `npData`, `npQueue`, and `npLogDelivery` read endpoint URLs (`url_conn` / `url_queue` / `url_delivery`) from `mailgw/config/logging.json` via Haraka's `this.config.get`, and POST JSON through `mailgw/plugins/functions.js#postWithLogging` (which writes a local log line, then calls `httplog`). Note that `npFilterAttach` instead uses **hardcoded** `http://localhost:3000` URLs.

#### Plugin inconsistencies & gotchas

The plugin set grew organically and is **not** uniform — don't assume symmetry. Known quirks (improvement items tracked in `mailgw/TODO.md`):

- **Name ≠ behavior.** `npConnection` does *not* POST to logservice — it only writes a local log file and has a dead `isBlacklistedIP()` placeholder (always `false`). The plugin that actually sends connection info to the API is `npData`, at the `hook_data` stage, posting to `url_conn`.
- **Config source split.** `npData` / `npQueue` / `npLogDelivery` resolve URLs from `logging.json`; `npFilterAttach` **hardcodes** `http://localhost:3000`, so its calls only work when logservice is co-located (e.g. they break in Docker where logservice is a separate host).
- **Overlapping event posting.** `npFilterAttach.hook_data_post` *also* posts connection (`url_conn`) and transaction (`url_queue`) events in addition to its MD5 check, overlapping `npData` / `npQueue` — a duplicate-row risk (the "no longer double-insert" note in `logservice/src/routes/api.ts` is a scar from this).
- **Posts are fire-and-forget.** `postWithLogging` / `httplog` don't `await` the POST; failures are only logged, never retried or surfaced to the SMTP transaction.
- **Plugins that never touch the API:** `npConnection` (local file), `npFilter` (IP allowlist; its `hook_queue_outbound` only logs rcpt locally), `npRoute` (routing; DEV-only local log).

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

`Bun.serve` app (`logservice/src/index.ts`) listening on `PORT` (default 3000). Routes:
- `GET  /` — health check, returns `{ status: "OK" }`
- `POST /api/connection` — inbound connection events; `GET /api/connection` — search
- `POST /api/queue` — queue events (stored as `Transaction` rows)
- `POST /api/delivery` — delivery events (validated with a Zod schema); `GET /api/delivery` — search
- `GET  /api/transaction` — search transactions
- `POST /filter/md5` — attachment MD5 blocklist check, returns `{ action: "allow" | "block" }`

Each handler is wrapped by `handle()` = `withAuth(withErrorHandling(...))` (`src/middleware/`). Auth checks the `X-API-Key` header against `API_KEY`; when `API_KEY` is unset, all requests are accepted. The `GET` search endpoints take a JSON `q` query param (`{ search: [{ field, operator, value }], searchLogic, limit, offset }`) parsed/whitelisted in `src/query/`.

Data access uses raw SQL via Bun's `Bun.SQL` (`src/db.ts`, MySQL adapter) — there is **no ORM**. Per-table query helpers live in `src/models/` (`connection.ts`, `transaction.ts`, `delivery.ts`, etc.). Schema migrations are plain numbered `.sql` files in `logservice/migrations/`, applied in order by the custom runner `src/dbmigrate.ts` (tracks applied files in a `_migrations` table). Migrations run via the `db-migrator` container, the `--migrate` boot flag (`index.ts`), or `bun run db:migrate`.

### DEV mode

Setting `MODE=DEV` (`pnpm dev`) enables extra JSON logging to `log/ngmroute.log` inside `npRoute.js`. Log files are written to `log/` relative to the Haraka working directory (`mailgw/` locally or `/opt/mailgw` in Docker).
