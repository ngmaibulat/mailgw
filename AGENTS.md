High-signal tips for automated agents working in this repo

Read these first (order matters):
- CLAUDE.md (contains the curated project overview and exact commands) — start here.
- pnpm-workspace.yaml and root package.json (shows which packages are pnpm vs Bun).

Exact commands you'll need
- Run the Haraka SMTP server (mailgw):
  - From repo root: `pnpm --filter @aibulat/mailgw start`
  - Or: `cd mailgw && pnpm start`
- Run logservice (Bun project): `cd logservice && bun run dev`
- Run the active web UI (Fastify): `pnpm webui:dev` or `pnpm --filter mailgw-webui-fastify dev`
- Generate TLS certs used by the webui: `pnpm certs` (root) or `cd certs && bun run generate`
- Run e2e tests (requires a running stack): `pnpm test:e2e` (runs `bun test tests/` from repo root)

Monorepo and package-manager gotchas (agents often miss this)
- This is a mixed pnpm + Bun repo: mailgw, webui-express, webui-fastify are pnpm workspace members; logservice, tests, and certs are Bun projects with their own bun.lock and are intentionally NOT pnpm workspace members. Do NOT try to run Bun projects using pnpm scripts that assume a Node toolchain.
- Root package.json scripts sometimes call Bun directly (e.g. `pnpm certs` executes `bun certs/src/generate.ts`).
- The repo pins a pnpm version in packageManager — use pnpm to run workspace scripts.

Web UI specific
- Prefer modifying webui-fastify (active rewrite). webui-express is legacy and kept for reference only.
- webui-fastify is TypeScript ESM but has no build step — Node 26 runs the .ts files directly. `pnpm --filter mailgw-webui-fastify typecheck` runs tsc if you need static checks.
- webui requires `./certs/server.{key,crt}` (by default under `certs/generated/webui`) on startup and will crash without them. Always run `pnpm certs` before `docker compose up` or before starting the webui locally.
- Sessions are in-memory (no external store) and users are seeded with `node create_user.ts` in the webui directory.

logservice and DB
- logservice is a Bun server using Bun.SQL (MySQL). Migrations live in `logservice/migrations/` and are run with `bun run db:migrate` (or via the db-migrator container). Use the db-migrator or `bun run start:migrate` to apply migrations.
- Tests that mutate DB or depend on a running stack are opt-in via env vars described in CLAUDE.md (`MAILGW_API_E2E`, `MAILGW_DB_CHECK`). Don't run DB-mutating e2e tests unless you intend to (they require a running MariaDB from docker-compose).

Haraka / mailgw runtime gotchas
- Haraka loads plugins listed in `mailgw/config/plugins` and expects runtime config files (connection.ini, routing.json, relays.json, logging.json, ngmfilter.json) to be mounted at `/opt/mailgw/config` in production. Those files are NOT in the repo.
- Plugin pitfalls agents commonly miss:
  - `npConnection.js` writes local logs only — it does NOT POST to logservice. The plugin that posts connection events is `npData.js`.
  - `npFilterAttach.js` hardcodes `http://localhost:3000` for its API calls; in Docker this breaks if logservice is remote — use/inspect `mailgw/config/logging.json` for the canonical endpoints used by other plugins.
  - Posts from `postWithLogging` / `httplog` are fire-and-forget (not awaited). Failures are only logged locally.

Docker / containers
- Container scripts (e.g. `mailgw/container-build.sh`) build from the repo-root context so the pnpm workspace lockfile is visible. `./docker-compose.yaml` expects `certs/generated/webui` to exist.
- The webui image built by `build:webui` is the Fastify one (webui-fastify). The legacy Express image is built separately and should not be the default target.

Tests and CI
- e2e tests live in `tests/` (Bun) and talk to a running stack (use `docker compose up -d`). Run them from repo root so Bun picks up the root .env.
- Unit tests for logservice live under `logservice/tests` and are run with `cd logservice && bun test tests/`.

Where to look first when you are confused
- CLAUDE.md (authoritative project overview). Then:
  - root package.json (scripts and pnpm intent), pnpm-workspace.yaml (package boundaries),
  - mailgw/ (Haraka plugins + config), logservice/ (Bun server + migrations), webui-fastify/ (active UI).

If uncertain, ask a one-line question instead of guessing (e.g. "Should I run DB migrations for this change?").
