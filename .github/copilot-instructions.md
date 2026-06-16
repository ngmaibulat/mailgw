Purpose

This file equips GitHub Copilot CLI / code assistants with concise, repo-specific context: how to build/test/lint, the high-level architecture, and repository conventions that must be known across files.

Quick commands (root)

- Install: pnpm install (repo uses pnpm workspace) and bun install inside Bun packages when needed.
- Start mailgw (Haraka): pnpm --filter @aibulat/mailgw start
- Dev mailgw (extra logging): pnpm --filter @aibulat/mailgw dev (MODE=DEV)
- Start web UI (fastify): pnpm webui:start or pnpm --filter mailgw-webui-fastify start
- Web UI dev: pnpm webui:dev (alias → pnpm --filter mailgw-webui-fastify dev)
- Generate certs: pnpm certs  (invokes Bun generator)
- Run all e2e: pnpm test:e2e
- Run only logservice e2e: pnpm test:e2e:api
- Run only smtp e2e: pnpm test:e2e:smtp
- Run logservice tests (Bun): cd logservice && bun test tests/
- Lint webui-fastify: pnpm --filter mailgw-webui-fastify lint
- Lint check/fix: pnpm --filter mailgw-webui-fastify check / check:fix
- Build containers: pnpm run build:containers (invokes package-specific container scripts)

Running a single test

- Bun-based tests (logservice or tests/): from repo root (so Bun loads root .env) run: bun test tests/api/logservice.e2e.test.ts or bun test tests/smtp/smtp.e2e.test.ts
- mailgw package tests (pnpm): cd mailgw && pnpm test <path/to/test-file.js> or use the workspace filter: pnpm --filter @aibulat/mailgw test -- <args>
- webui-fastify (node --test): cd webui-fastify && node --test path/to/test.ts

High-level architecture (big picture)

- Monorepo split: pnpm workspace for Node packages (mailgw, webui-fastify, webui-express) + standalone Bun packages (logservice, tests, certs). Keep this split in mind: Bun vs pnpm tooling and package managers coexist intentionally.

- mailgw: Haraka-based SMTP router (mailgw/). Custom plugins live in mailgw/plugins and are loaded via mailgw/config/plugins. Plugins use Haraka hooks to produce events and route mail via RoutingTable.

- logservice: Bun HTTP API (logservice/) — receives JSON events (connection/queue/delivery), stores them in MariaDB via Bun.SQL, exposes search endpoints and an attachment MD5 filter endpoint (/filter/md5). Uses migrations in logservice/migrations/.

- webui-fastify: Active admin UI (webui-fastify/) — TypeScript run directly on Node 26 (no build). It is a read-only frontend for logs: GET /api/* routes are proxied to logservice (the webui does not query log tables directly). It also manages relay config (Drizzle) for the UI's owned tables.

- webui-express: Legacy reference; contains ingestion endpoints and duplicate models — prefer changes in webui-fastify.

- tests/: Bun e2e suites that operate against a running stack (docker compose up -d). Run from repo root so root .env is available.

Key conventions and gotchas (must-know)

- Haraka plugins: custom plugins are prefixed with np (e.g., npData.js, npQueue.js). Hook mapping is important: e.g., npData uses hook_data to POST connection events; npConnection writes a local log and does NOT POST.

- Logging endpoints config: mailgw plugins (npData/npQueue/npLogDelivery) read url_conn/url_queue/url_delivery from mailgw/config/logging.json. BUT npFilterAttach hardcodes http://localhost:3000 — this causes environment-dependent failures (Docker vs local).

- Posts are fire-and-forget: postWithLogging/httplog do not await POSTs. Failures are logged locally only.

- Routing rules: mailgw loads routing.json and relays.json and uses RoutingTable.findRoute(sender, rcpt) which walks rules in order and returns the first match; empty string means wildcard.

- Runtime configs: files like connection.ini, routing.json, relays.json, logging.json, ngmfilter.json are runtime-mounted (often at /opt/mailgw/config) and are not in the repo.

- Bun vs pnpm: logservice, tests, certs are Bun projects (bun.lock present). webui-fastify and mailgw are pnpm/Node. Use the matching toolchain when running or editing those packages.

- webui-fastify specifics: TypeScript ESM, run by Node 26 with native type-stripping (no build). Biome for linting; drizzle-zod produces zod v4 schemas while rest of app uses zod v3.

- Sessions: webui sessions are in-memory. create_user.ts seeds users.

Where to look first for common tasks

- plugins and routing: mailgw/plugins/ and mailgw/config/
- log API and migrations: logservice/src/ and logservice/migrations/
- web UI: webui-fastify/src/, especially src/logservice.ts (proxy logic) and src/routes/
- e2e tests: tests/ and logservice tests under logservice/tests/

AI assistant files to incorporate

- CLAUDE.md (detailed, authoritative) — used as the primary source for architecture and gotchas.
- .claude (local assistant settings) exists; do not overwrite.

Notes for Copilot sessions

- Prefer operating from repo root for e2e/test tasks so Bun loads root .env.
- Respect the Bun/vs-pnpm split when running commands or installing deps.
- When modifying mailgw plugin behavior, check logging.json vs hardcoded URLs (npFilterAttach) to avoid Docker-only bugs.

If you'd like, configure MCP servers (e.g., Playwright) for web UI testing — reply yes and which server to add.

Summary

Created focused, repo-specific Copilot instructions describing commands, architecture, and conventions. Ask if any additional areas (e.g., deployment, CI workflow, or Playwright setup) should be covered.