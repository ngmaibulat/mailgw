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
- Unit tests: validation, query builder, auth.

## Remaining work

1. **Live-DB integration tests** (highest value)
   Current tests are unit-level only. Add smoke tests that run the 3 POST
   endpoints against a real MariaDB (the `docker compose` db) and assert rows
   are actually written. This is the main confidence gap before production.

2. **Attachment hash-check feature** (only if attachment blocking is used)
   - Port `checkMD5` / `hashLookup` / `hashListLookup` orchestration from the
     old `functions.js` (models `BlockMD5` / `HashLookup` already exist).
   - Add the missing `/filter/md5` route: the Haraka `npFilterAttach` plugin
     POSTs to `http://localhost:3000/filter/md5`, but `index.ts` exposes no
     such endpoint — the feature can't work until this is added.

3. **Deferred models / endpoints** (do only when needed)
   `Header`, `Log`, `Config`, `RelayGroup`, `Relay`, `User`, `Exception` — no
   active routes use them yet; port on demand.

## Out of scope here

Haraka plugin hardening (relaying allowlist, fail-open attachment scanner,
case-insensitive routing) is tracked separately in `planning/next-02.md`.
