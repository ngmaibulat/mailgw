# webui-fastify TODO

Improvement backlog for the active admin UI (Fastify + Drizzle, pug-only,
native HTTP/2, **TypeScript**). Items derived from reading the current code;
file refs are where the work lands.

**Legend:** `[x]` done · `[~]` partial / stubbed · `[ ]` not started

---

## Snapshot — what exists today

- **TypeScript** throughout (`.ts`), run directly by Node 26 via native type-stripping — no build step / no emit. `tsconfig.json` (`verbatimModuleSyntax` + `erasableSyntaxOnly`) is for `pnpm typecheck` + editor only; relative imports carry the real `.ts` extension. `typescript`/`@types/*` are devDeps, omitted from the prod image.
- **Biome** lint + format (`biome.json`, recommended rules, style matched to the old prettier/editorconfig). Scoped to `src`/`db`/root `*.ts`+`*.json`; `public/lib` (vendored) and `public/js` (browser scripts) are excluded; import-sort assist off (keeps manual import groups). Scripts: `lint`, `format`, `check`, `check:fix`. CI should gate on `pnpm typecheck` + `pnpm check` (type-stripping does not type-check at runtime).
- Native HTTP/2 server (`src/index.ts`, Fastify `{ http2, https: { allowHTTP1 } }`); requires `./certs/server.{key,crt}`.
- Session login (`/login`, bcrypt vs `User.hash`), in-memory session store, `checkSession` preHandler guard.
- Encapsulated auth scopes in `src/app.ts` (public static → logged → secured).
- Log viewer pages (pug): connection, delivery, mails, lookups.
- Read-only `/api/{connection,delivery,queue,hashlookups}` — **proxied to logservice** (`src/logservice.ts`); no ingest API, no `/filter/md5`.
- Relay & relay-group CRUD under `/config` (`CtrlRelay`, `CtrlRelayGroup`), validated with drizzle-zod.
- **Drizzle ORM** (`db/schema.ts` + `db/index.ts`), scoped to what the webui owns: `users`, `relays`, `relayGroups`, `logs`, `exceptions`. The webui does not own/migrate the schema (logservice does).
- CLI user tools: `create_user.ts`, `check_user.ts`.

---

## High priority — bugs & security

- [x] **Relay edit wipes `auth_pass` (data loss).** Fixed in the Drizzle migration: `CtrlRelay.editHandle` drops an empty `auth_pass` from the update set ("leave blank to keep"), and the form shows a placeholder hint.
- [ ] **Sessions never expire and logout is weak.** `src/globals.ts` (`sessions = {}`) is never pruned; `src/auth/login.ts` adds entries but `src/auth/logout.ts` only clears the cookie — server-side session lives forever (memory leak + still valid if cookie re-presented). Store `{ email, expiresAt }`, check expiry in `checkSession`, `delete` on logout, sweep periodically — or move to `@fastify/session` + a real store (also enables restart-survival / multi-instance).
- [x] **No validation on config mutations (mass-assignment).** Fixed: `src/validation/config.ts` derives drizzle-zod insert schemas (`.pick()` to form fields → strips extras; `.extend()` requires `name`/`host`); controllers `safeParse` and re-render the form with an error on failure.
- [ ] **`auth_pass` stored plaintext** (`db/schema.ts`, `relays.auth_pass` `varchar`) and shown in the form. Encrypt at rest.
- [ ] **Weak cookie secret default.** `src/app.ts:44` `SIGN_COOKIE || "sign"`. Require it via `src/checkenv.ts` (or refuse to boot in `NODE_ENV=production`).
- [ ] **No brute-force protection on `/login`.** Add `@fastify/rate-limit`.
- [ ] **No security headers.** Add `@fastify/helmet` (CSP/HSTS/X-Frame-Options).

## Medium — robustness

- [x] **Search/query requests are forwarded unvalidated.** Fixed: `src/validation/search.ts` defines a zod v4 `searchRequest` schema mirroring logservice's accepted shape (`{ search: [{ field, operator, value }], searchLogic, limit, offset }`, operators + `AND`/`OR` enumerated, `value` scalar-or-2-tuple, non-negative int `limit`/`offset`). `parseSearchRequest` decodes the `?request=<json>` string, `safeParse`s it, and returns either the re-serialized (unknown-keys-stripped) JSON to forward or an error; `src/routes/api.ts`'s `proxySearch` returns **400** on failure instead of bouncing a bad request off logservice. logservice still does the authoritative field whitelist + parse (defense-in-depth).
- [ ] **Proxy has no timeout and hard-500s when logservice is down** (`src/logservice.ts`: `fetch` then throw on `!ok`). Add `signal: AbortSignal.timeout(...)` and return a friendly empty/error grid payload instead of a 500.
- [x] **Stale schema-check magic number.** Gone with the Sequelize removal — the brittle `< 15` table-count check no longer exists (the webui doesn't inspect/own the schema).
- [x] **DB connect + `process.exit` at import time.** Fixed: `db/index.ts` is a lazy `mysql2` pool (no import-time connect); `src/index.ts` calls `assertDbConnection()` (a ping) at startup and exits only there on failure.
- [ ] **No `setErrorHandler`.** Handler exceptions fall through to Fastify's default 500; currently relying on `process.on("uncaughtException")` in `src/errhandler.ts` (blunt). Add a Fastify error handler.
- [ ] **`trustProxy`** — if deployed behind a TLS-terminating reverse proxy, set `Fastify({ trustProxy: true })` so the `secure` cookie and `request.ip` (used by `src/middleware/logger.ts`) are correct.

## Operations

- [ ] **Graceful shutdown.** Handle `SIGTERM`/`SIGINT` → `app.close()` + `closeDb()` (Docker stop is currently abrupt).
- [ ] **Health endpoint.** `/` is the protected dashboard — add an unauthenticated `GET /health` for Docker/k8s liveness.
- [ ] **Unbounded request logging.** `logger:false` (`src/index.ts`) disables Fastify's pino; `src/middleware/logger.ts` writes every request to the `Logs` table fire-and-forget, forever (no retention/sampling, errors swallowed). Consider enabling pino for request logs and making DB logging sampled/optional + a retention policy.

## Testing / DX

- [ ] **Add route tests via `app.inject()`.** `src/app.ts#build()` returns the app, so the auth gate, redirects, and proxy path-mapping can be tested with no live server (mock `logservice.search` + the `db`). High value now that the app is ~845 lines.

## Features (carried over from webui-express, still open)

- [~] User management — CLI only (`create_user.ts`); no web UI.
- [ ] `/profile` — route renders `util/notimpl`.
- [ ] `/config/routing` — renders `util/notimpl`; routing rules are not editable from the UI.
- [ ] Roles / per-user log scoping — `User` model has only `email` + `hash`.
