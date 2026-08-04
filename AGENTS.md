High-signal tips for automated agents working in this repo

Read these first (order matters):
- CLAUDE.md (contains the curated project overview and exact commands) — start here.
- pnpm-workspace.yaml and root package.json (shows which packages are pnpm vs Bun).

Exact commands you'll need
- Run the gateway (mailgw-go, the current one): `pnpm start` (or `pnpm dev` to validate the config first)
- Run the legacy Haraka server: `pnpm legacy:start` — needs a standalone `cd legacy/mailgw && pnpm install` first (it is NOT a workspace member)
- Run logservice (Bun project): `cd logservice && bun run dev`
- Run the active web UI (Fastify): `pnpm webui:dev` or `pnpm --filter mailgw-webui-fastify dev`
- Generate TLS certs used by the webui: `pnpm certs` (root) or `cd certs && bun run generate`
- Run e2e tests (requires a running stack): `pnpm test:e2e` (runs `bun test tests/` from repo root)
- Run a docs site: `pnpm docs:public:dev` (5173), `pnpm docs:internal:dev` (5174), `pnpm docs:testing:dev` (5175); `pnpm docs:build` builds all three

Monorepo and package-manager gotchas (agents often miss this)
- This is a mixed pnpm + Bun + Go repo: webui-fastify and the three docs/* sites are the pnpm workspace members; logservice, tests, and certs are Bun projects with their own bun.lock; mailgw-go is a Go module. Everything under legacy/ (legacy/mailgw, legacy/webui-express) is frozen and deliberately outside the workspace — install those standalone. Do NOT try to run Bun projects using pnpm scripts that assume a Node toolchain.
- Root package.json scripts sometimes call Bun directly (e.g. `pnpm certs` executes `bun certs/src/generate.ts`).
- The repo pins a pnpm version in packageManager — use pnpm to run workspace scripts.

Web UI specific
- Prefer modifying webui-fastify (active). legacy/webui-express is frozen and kept for reference only.
- webui-fastify is TypeScript ESM but has no build step — Node 26 runs the .ts files directly. `pnpm --filter mailgw-webui-fastify typecheck` runs tsc if you need static checks.
- webui serves TLS from `./certs/server.{key,crt}` (by default under `certs/generated/webui`) and **self-signs a pair there if the directory is empty**, so `docker compose up` works with no preparation. `pnpm certs` is optional and gives it the repo's pair instead; an existing pair is never overwritten.
- Sessions are database-backed (`Sessions` table). The first admin is created via the one-time `/setup` page or `node create_user.ts` in the webui directory.

logservice and DB
- logservice is a **Go** server (`logservice-go/`) over MariaDB via `database/sql`. Migrations live in `logservice-go/migrations/` as `.sql` files embedded with `go:embed`, and are applied **automatically on start** — the only thing that migrates the shared schema since M22 deleted the `db-migrator` service. `logservice migrate` applies them and exits; nothing in compose runs it, `deploy/core/upgrade.sh` does. The console waits for its own tables at boot (`webui-fastify` `db/index.ts` `waitForSchema`). The Bun original is frozen at `legacy/logservice/`.
- Tests that mutate DB or depend on a running stack are opt-in via env vars described in CLAUDE.md (`MAILGW_API_E2E`, `MAILGW_DB_CHECK`). Don't run DB-mutating e2e tests unless you intend to (they require a running MariaDB from docker-compose).

Haraka / mailgw runtime gotchas
- Haraka loads plugins listed in `legacy/mailgw/config/plugins` and expects runtime config files (connection.ini, routing.json, relays.json, logging.json, ngmfilter.json) to be mounted at `/opt/mailgw/config` in production. Those files are NOT in the repo.
- Plugin pitfalls agents commonly miss:
  - `npConnection.js` writes local logs only — it does NOT POST to logservice. The plugin that posts connection events is `npData.js`.
  - `npFilterAttach.js` hardcodes `http://localhost:3000` for its API calls; in Docker this breaks if logservice is remote — use/inspect `legacy/mailgw/config/logging.json` for the canonical endpoints used by other plugins.
  - Posts from `postWithLogging` / `httplog` are fire-and-forget (not awaited). Failures are only logged locally.

Docker / containers
- Container scripts (e.g. `webui-fastify/container-build.sh`) build from the repo-root context; logservice and mailgw-go build from their own dirs. The legacy ones under `legacy/<pkg>/` cd two levels up to reach the root context. `./docker-compose.yaml` expects `certs/generated/webui` to exist.
- The webui image built by `build:webui` is the Fastify one (webui-fastify). The legacy Express image is built separately and should not be the default target.
- `pnpm run docker:push` bumps + pushes the newer stack only — mailgw-go, logservice, webui-fastify. `pnpm build:containers` pushes those plus the legacy Haraka `mailgw` image.

Tests and CI
- e2e tests live in `tests/` (Bun) and talk to a running stack (use `docker compose up -d`). Run them from repo root so Bun picks up the root .env.
- Unit tests for logservice live under `logservice-go/internal/**/*_test.go` and are run with `cd logservice-go && go test -race ./...`. The frozen Bun suite is `cd legacy/logservice && bun test tests/`.

Where to look first when you are confused
- CLAUDE.md (authoritative project overview). Then:
  - docs/internal/ for architecture and conventions, docs/public/ for what the gateway does, docs/testing/ for manual test plans,
  - root package.json (scripts and pnpm intent), pnpm-workspace.yaml (package boundaries),
  - mailgw-go/ (the gateway), logservice-go/ (log API + migrations), webui-fastify/ (active UI), deploy/ (production compose), legacy/ (frozen Haraka stack + the Bun logservice).

If uncertain, ask a one-line question instead of guessing (e.g. "Should I run DB migrations for this change?").
