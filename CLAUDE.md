# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a mail gateway/router built on [Haraka](https://haraka.github.io/) (Node.js SMTP server). It accepts inbound SMTP, applies routing rules to forward mail to configured relay targets, and POSTs structured JSON events to a companion logging service.

The repo is a monorepo with a private root `package.json`. `mailgw/`,
`webui-fastify/`, and `webui-express/` are the pnpm workspace members (see
`pnpm-workspace.yaml`); `logservice/`, `tests/`, and `certs/` are standalone
**Bun** packages with their own `bun.lock`, deliberately kept out of the pnpm
workspace so pnpm and Bun don't fight over `node_modules`.
- **`mailgw/`** — the Haraka SMTP server with custom plugins (`mailgw/plugins/`); a Node.js / pnpm package
- **`logservice/`** — a Bun HTTP API (`Bun.serve`) that receives events from the plugins and stores them in MariaDB via Bun's native SQL client (`Bun.SQL`, MySQL adapter); written in TypeScript
- **`webui-fastify/`** — the **active** admin web UI (Fastify + Drizzle, Node/pnpm): log viewers, relay/relay-group config, session login. Native HTTP/2, pug-only. Its log-viewer reads **proxy to logservice** (it no longer queries the log tables directly); the build/compose stack ships this image. See its section below.
- **`webui-express/`** — the **legacy** Express rewrite target, kept as a reference. Same product on Express 5 over HTTP/1.1. It still carries the pre-refactor baggage `webui-fastify` shed (dual pug/ejs engines, the experimental Deno adapter, the full duplicated model set, and the overlapping `/api` ingest + `/filter/md5` endpoints). Prefer changing `webui-fastify`.
- **`tests/`** — cross-cutting end-to-end tests (Bun): logservice API (`tests/api/`) and SMTP pipeline (`tests/smtp/`)
- **`certs/`** — a Bun CLI that generates self-signed TLS certs from a JSON config (`node-forge`); produces the certs the webui serves over TLS

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

### webui-fastify (active admin UI)

A pnpm workspace member (`mailgw-webui-fastify`). The root `pnpm webui*` scripts target it:

```bash
pnpm webui:dev                             # nodemon (auto-reload)  → src/index.ts  (alias for pnpm --filter mailgw-webui-fastify)
pnpm webui:start                           # node src/index.ts (production)
pnpm webui dev                             # same as webui:dev (the `webui` alias forwards args)
pnpm --filter mailgw-webui-fastify test    # node --test  (no test files yet)
pnpm --filter mailgw-webui-fastify typecheck  # tsc --noEmit (types only; runtime needs no build)
pnpm --filter mailgw-webui-fastify lint       # biome lint
pnpm --filter mailgw-webui-fastify check      # biome check (lint + format check, no write)
pnpm --filter mailgw-webui-fastify check:fix  # biome check --write (apply safe fixes + format)
cd webui-fastify && node create_user.ts <email> <password>   # seed a login user
```

> Written in **TypeScript** (`.ts`) and run directly by Node 26 via native type-stripping — **no build step / no emit**; `tsconfig.json` is for IntelliSense + `pnpm typecheck` only (`typescript` is a devDep, not needed at runtime, so the prod Docker image omits it). Source uses `verbatimModuleSyntax` + `erasableSyntaxOnly`, so relative imports carry the real `.ts` extension (`import { build } from "./app.ts"`). Lint + format is **Biome** (`biome.json`, recommended rules; 4-space / double-quote / 80-col / semicolons to match the old `.editorconfig`). Biome scopes to `src`/`db`/root `*.ts`/`*.json` (vendored `public/lib` and browser `public/js` are excluded); its import-sorting assist is **off** so the node/third-party/local import grouping is preserved. Unused Fastify handler params follow the `_`-prefix convention. Serves **native HTTP/2** (Fastify `{ http2: true, https: { allowHTTP1: true } }`) and reads `./certs/server.{key,crt}` (relative to its working dir) on boot — it will crash without those certs. Generate them with the `certs/` project (see below); locally a `certs` symlink points at `certs/generated/webui`, and in Docker the same dir is mounted. Templates are **pug-only** (`TEMPLATE_DIR`, default `./templates/pug`). Log-viewer reads proxy to logservice via `LOGSERVICE_URL` (default `http://localhost:3000`) + optional `LOGSERVICE_API_KEY` (sent as `X-API-Key`).

### webui-express (legacy reference)

The older Express version, kept side-by-side as a reference (workspace member `mailgw-webui`). Run it via its explicit filter (the root `pnpm webui*` aliases now point at `webui-fastify`):

```bash
pnpm --filter mailgw-webui dev          # nodemon (auto-reload)  → src/index.mjs
pnpm --filter mailgw-webui start        # node src/index.mjs (production)
cd webui-express && node create_user.mjs <email> <password>   # seed a login user
```

> Serves **HTTP/1.1 over TLS** (`node:https`, `src/index.mjs`) — Express is not compatible with Node's native http2 server, so HTTP/2 is intentionally Fastify-only. Reads the same `./certs/server.{key,crt}`. Templates default to pug (`TEMPLATE_ENGINE=pug|ejs`).

### certs

```bash
cd certs && bun install      # first time only
pnpm certs                   # generate from certs/certs.config.json (or: cd certs && bun run generate)
bun certs/src/generate.ts path/to/other.json   # explicit config path
```

Reads `certs/certs.config.json` (committed; `defaults` merged into each `certs[]` entry) and writes a self-signed key+cert per entry under its `out` dir. Generated files (default `certs/generated/...`) are **gitignored**. The shipped `webui` entry produces `certs/generated/webui/server.{key,crt}`, which `docker-compose.yaml` mounts into the webui container.

### Container / Docker

Each Node/Bun service has `container-build.sh` (local `--load`) and
`container-push.sh` (build + push) scripts that bump the package version and tag
`ngmaibulat/<name>:v<ver>` + `:latest`. The mailgw and webui images build from
the **repo-root context** (so the workspace `pnpm-lock.yaml` is visible) via
`-f <pkg>/Dockerfile`; logservice builds from its own dir.

```bash
./mailgw/container-build.sh        # mailgw image (node:26-alpine, -f mailgw/Dockerfile, root context)
./mailgw/container-push.sh         # build + push to Docker Hub (ngmaibulat/mailgw)
cd mailgw && ./container-dev.sh    # run latest image locally (mounts mailgw/plugins live)
./webui-fastify/container-build.sh    # ACTIVE webui image (ngmaibulat/mailgw-webui-fastify, node:26-alpine, root context)
./webui-express/container-build.sh    # legacy Express webui image (ngmaibulat/mailgw-webui)
./logservice/container-build.sh       # logservice image (oven/bun-alpine, own-dir context)
pnpm build:containers              # push mailgw + logservice + webui (build:webui → webui-fastify)
docker compose up                  # full stack: mariadb + db-migrator + logservice + mailgw + webui + mailhog
docker compose run --rm db-migrator   # apply SQL migrations against MariaDB (runs `bun src/dbmigrate.ts`)
```

> **`docker-compose.yaml`'s `webui` service runs the `mailgw-webui-fastify` image** (with `LOGSERVICE_URL`/`LOGSERVICE_API_KEY` wired for the read proxy). The root `pnpm build:webui` / `build:containers` scripts build/push that same Fastify image. The legacy Express image (`ngmaibulat/mailgw-webui`) is built only via `./webui-express/container-build.sh`.

> Before `docker compose up`, generate the webui TLS certs (`pnpm certs`) — the
> `webui` service mounts `certs/generated/webui` and won't start without them.

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
- `GET  /api/hashlookup` — search attachment MD5 lookups; `HashLookups` LEFT JOIN `Transaction` (on `txn_uuid = uuid`) so each row carries its message's `sender`/`rcpt_list`/`dt` (`searchHashlookup` in `src/query/search.ts`). Only `HashLookups` columns are searchable
- `POST /filter/md5` — attachment MD5 blocklist check, returns `{ action: "allow" | "block" }`

Each handler is wrapped by `handle()` = `withAuth(withErrorHandling(...))` (`src/middleware/`). Auth checks the `X-API-Key` header against `API_KEY`; when `API_KEY` is unset, all requests are accepted. The `GET` search endpoints take a JSON `q` query param (`{ search: [{ field, operator, value }], searchLogic, limit, offset }`) parsed/whitelisted in `src/query/`. `buildWhere` takes an optional `tablePrefix` to qualify columns for JOIN queries (used by the hashlookup search).

Data access uses raw SQL via Bun's `Bun.SQL` (`src/db.ts`, MySQL adapter) — there is **no ORM**. Per-table query helpers live in `src/models/` (`connection.ts`, `transaction.ts`, `delivery.ts`, etc.). Schema migrations are plain numbered `.sql` files in `logservice/migrations/`, applied in order by the custom runner `src/dbmigrate.ts` (tracks applied files in a `_migrations` table). Migrations run via the `db-migrator` container, the `--migrate` boot flag (`index.ts`), or `bun run db:migrate`.

### webui-fastify (active admin UI)

Fastify app (`webui-fastify/src/app.ts#build()`, started over native HTTP/2 by `src/index.ts`). **TypeScript ESM throughout (`.ts`)**, run directly via Node's native type-stripping (no build step — see the webui-fastify command section above). Composition:
- **Plugins** registered at the root (inherited by all routes): `@fastify/formbody` (urlencoded), `@fastify/cookie` (signed, via `SIGN_COOKIE`), `@fastify/view` (pug), `@fastify/static` (`public/`, `index:false` so the dashboard owns `/`).
- **Encapsulation = the auth boundary.** Three nested scopes: (1) root — static assets, public & unlogged; (2) a "logged" child that adds the `logger` `onRequest` hook and the public auth routes (`/login`, `/logout`, `/profile`); (3) a "secured" child inside it that adds the `checkSession` `preHandler` hook, then registers the protected routes. Hooks only apply to their own scope + descendants, so static stays unlogged and auth routes stay ungated — replacing Express's ordering-dependent `app.use()` chain.
- **Routes** (`src/routes/`): `root` (dashboard), `log` (viewer pages), `api` (`/api/{connection,delivery,queue,hashlookups}` — **read-only**, proxied), `config-relay` (`/config/relay/*`, `/config/relaygrp/*` CRUD; `/config/routing` is a `notimpl` stub). There is **no ingest API and no `/filter/md5`** — those live in logservice.
- **Read proxy** (`src/routes/api.ts` + `src/logservice.ts`): the `/api/*` GETs validate the frontend's `?request=<json>` against a zod v4 schema (`src/validation/search.ts`, mirrors logservice's accepted shape — bad payload → **400**, unknown keys stripped), then forward it as logservice's `?q=<json>` and pass the response through verbatim (identical `{status,total,records}` shape). Path remaps: `queue` → logservice `/api/transaction`, `hashlookups` → `/api/hashlookup`. The webui **does not query the log tables directly**.
- **Auth** (`src/auth/`): session login (`bcryptjs` vs `User.hash`), `/login` `/logout` `/profile`. Sessions are **in-memory** (`src/globals.ts`); `checkSession` unsigns the cookie via `request.unsignCookie`. `/profile` is a `notimpl` stub. Users are seeded only via `create_user.ts`.
- **Data**: **Drizzle ORM** (MariaDB via `mysql2`), **scoped to only what the webui owns** — `users`, `relays`, `relayGroups`, `logs`, `exceptions` (`db/schema.ts`). The webui **does not own/migrate this schema** — logservice's SQL migrations create the tables; `db/schema.ts` just describes the columns to query (no `drizzle-kit`/DDL here). `db/index.ts` exports the `db` instance (lazy `mysql2` pool, no import-time connect) + the table refs + `assertDbConnection()` (pinged at startup in `src/index.ts`). `src/adapter.ts` is now just a tiny re-export of uuid/bcrypt.
- **Config validation**: `src/validation/config.ts` uses **drizzle-zod** (`createInsertSchema`) to derive relay/relaygroup insert schemas from the Drizzle tables, `.pick()`ed to form fields (prevents mass-assignment) and `.extend()`ed to require `name`/`host`. The whole app is on **zod v4** — imported from `zod/v4` (the v4 entrypoint shipped by zod 3.25+), which is also what drizzle-zod 0.8 emits, so schemas and hand-written validators (`src/validation/{config,login}.ts`) all use one zod. Relay edit uses a "leave blank to keep" rule so editing never wipes the stored `auth_pass`.

Improvement backlog: `webui-fastify/TODO.md`.

### webui-express (legacy reference)

The earlier Express version, retained as a reference; **prefer `webui-fastify` for changes.** Express 5 app (`src/app.mjs`) served over **HTTP/1.1 by `node:https`** (`src/index.mjs`) — Express can't use Node's native http2. It still has the baggage the Fastify rewrite removed:
- **Routes**: same paths but the `/api/*` handlers still **ingest** (`create()` from `req.body`, no validation/`await`) in addition to querying, plus a `filter` route (`POST /filter/md5`) — the overlap with logservice.
- **Views**: two engines in parallel — pug (`templates/pug/`, default) and ejs (`templates/ejs/`), selected by `TEMPLATE_ENGINE`.
- **Data**: the **full** Sequelize model set, duplicating logservice's schema (Connection/Delivery/Transaction/hashlookups/…).
- **Runtime adapter**: `src/adapter.js` re-exports `adapter.node.js`; a parallel `adapter.deno.js` + `src/deno-index.mjs` exist for an abandoned Deno experiment. Roadmap in `webui-express/TODO.md` (older notes under `webui-express/archive/docs/`).

### DEV mode

Setting `MODE=DEV` (`pnpm dev`) enables extra JSON logging to `log/ngmroute.log` inside `npRoute.js`. Log files are written to `log/` relative to the Haraka working directory (`mailgw/` locally or `/opt/mailgw` in Docker).
