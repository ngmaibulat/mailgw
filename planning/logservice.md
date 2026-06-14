# logservice-bun — plan

High-level plan derived from `planning/next.md`, reconciled with the current
code (`logservice-bun/`). Most of the original `next.md` list is already done;
this file tracks only what remains. Detailed task-level status lives in
`logservice-bun/TODO.md`.

## Done (for reference)

- Migrations: 14 `.sql` files + `migrate.ts` runner (`--migrate` on startup).
- Persistence wired for `/api/connection`, `/api/queue`, `/api/delivery`.
- Auth (X-API-Key middleware) and centralized error-handling middleware.
- Dockerfile + `.dockerignore` present.
- Parameterized query builder with a column allowlist (injection guard).
- Attachment MD5 blocklist: `/filter/md5` route + `hashListLookup` orchestration
  (`decideActions` pure logic + `BlockMD5` / `HashLookup` models).
- Unit tests: validation, query builder, auth, hash decision logic.

Note: fixed the `SQL` import in `db.ts` / `src/migrate.ts` — `"bun:sql"` does
not resolve in Bun 1.3.13; the `SQL` class is imported from `"bun"`. The
service could not load before this.

## Remaining work

1. **Live-DB integration tests** (highest value)
   Current tests are unit-level only. Add smoke tests that run the POST
   endpoints (including `/filter/md5`) against a real MariaDB (the
   `docker compose` db) and assert rows are actually written. This is the main
   confidence gap before production.

2. **Deferred models / endpoints** (do only when needed)
   `Header`, `Log`, `Config`, `RelayGroup`, `Relay`, `User`, `Exception` — no
   active routes use them yet; port on demand.

## Out of scope here

Haraka plugin hardening (relaying allowlist, fail-open attachment scanner,
case-insensitive routing) is tracked separately in `planning/next-02.md`.
