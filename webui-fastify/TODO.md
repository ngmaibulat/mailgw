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
- [x] **Sessions never expire and logout is weak.** Fixed: `src/globals.ts` now stores `{ email, expiresAt }` (TTL `SESSION_TTL_MS` = 8h, synced with the login cookie's `maxAge`). `getSession()` treats expired entries as absent and prunes on access (re-presented stale cookie no longer authenticates); `checkSession` uses it. `src/auth/logout.ts` unsigns the cookie and `deleteSession()`s the server-side entry. `src/app.ts` runs an unref'd `sweepSessions()` interval (cleared on close) so the store can't grow unbounded. (A move to `@fastify/session` + a real store for restart-survival / multi-instance remains a future option.)
- [x] **No validation on config mutations (mass-assignment).** Fixed: `src/validation/config.ts` derives drizzle-zod insert schemas (`.pick()` to form fields → strips extras; `.extend()` requires `name`/`host`); controllers `safeParse` and re-render the form with an error on failure.
- [ ] **`auth_pass` stored plaintext** (`db/schema.ts`, `relays.auth_pass` `varchar`) and shown in the form. Encrypt at rest.
- [x] **Weak cookie secret default.** `SIGN_COOKIE` is already in `src/checkenv.ts`'s `REQUIRED` list, so the server refuses to boot without it. The misleading `|| "sign"` fallback in `src/app.ts` (dead in the prod path) is now removed — `build()` throws if `SIGN_COOKIE` is unset, so non-checkenv paths (e.g. tests) can't run on a hardcoded secret either. Residual (minor): no *strength*/min-length check on the value.
- [x] **No brute-force protection on `/login`.** Done: `@fastify/rate-limit` registered in `src/app.ts` with `global: false` (gates only opted-in routes), and `POST /login` opts in via `config.rateLimit` (`LOGIN_RATE_MAX` req / `LOGIN_RATE_WINDOW`, default 5 / "1 minute", keyed per `request.ip` — correct behind `trustProxy`). On exceed the plugin throws a 429 that lands in the existing `setErrorHandler`: browser form posts are redirected to `/login?msg=TooManyAttempts` (styled alert via `public/js/login.js`), JSON/API clients keep the 429. Covered by an `app.inject()` test.
- [x] **Security headers.** Done: `@fastify/helmet` registered at the root in `src/app.ts`, so every response (static, `/health`, all routes) gets HSTS, `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, COOP/CORP, etc. **enforced**. CSP is shipped **report-only** with a policy tailored to this UI (`script-src 'self'` — all scripts are same-origin `/lib`+`/js`; `style-src 'self' 'unsafe-inline'` — inline `style=` attrs; `img-src 'self' data:` — w2ui icons; `object-src 'none'`; `frame-ancestors 'none'`). Residual: verify no violations in the browser console (chiefly whether w2ui/jQuery needs `'unsafe-eval'`), then flip `reportOnly` to `false` to enforce. Covered by an `app.inject()` header test.

## Medium — robustness

- [x] **Search/query requests are forwarded unvalidated.** Fixed: `src/validation/search.ts` defines a zod v4 `searchRequest` schema mirroring logservice's accepted shape (`{ search: [{ field, operator, value }], searchLogic, limit, offset }`, operators + `AND`/`OR` enumerated, `value` scalar-or-2-tuple, non-negative int `limit`/`offset`). `parseSearchRequest` decodes the `?request=<json>` string, `safeParse`s it, and returns either the re-serialized (unknown-keys-stripped) JSON to forward or an error; `src/routes/api.ts`'s `proxySearch` returns **400** on failure instead of bouncing a bad request off logservice. logservice still does the authoritative field whitelist + parse (defense-in-depth).
- [x] **Proxy timeout.** Fixed: `src/logservice.ts` passes `signal: AbortSignal.timeout(TIMEOUT_MS)` (env `LOGSERVICE_TIMEOUT_MS`, default 10s) to the upstream `fetch`; a timeout surfaces as a `LogserviceError` of `kind: "network"` → **504** (with a "timed out after Nms" reason), so a hung logservice no longer hangs the webui request. (Hard-500 mapping to 502/504 was already in place.) Could still optionally return a friendly empty grid payload instead of an error.
- [x] **Stale schema-check magic number.** Gone with the Sequelize removal — the brittle `< 15` table-count check no longer exists (the webui doesn't inspect/own the schema).
- [x] **DB connect + `process.exit` at import time.** Fixed: `db/index.ts` is a lazy `mysql2` pool (no import-time connect); `src/index.ts` calls `assertDbConnection()` (a ping) at startup and exits only there on failure.
- [x] **`setErrorHandler`.** Added in `src/app.ts`: handler exceptions render `util/error.pug` (a friendly HTML page, generic message for 5xx so internals aren't leaked, the real message for 4xx) instead of Fastify's bare-JSON 500. API/JSON clients (`Accept: application/json` or `/api/*`) still get `{ status, message }` JSON. The process-level `uncaughtException`/`unhandledRejection` handlers in `src/errhandler.ts` remain for top-level safety.
- [x] **`trustProxy`.** Done, config-driven: `src/app.ts` reads `TRUSTED_PROXIES` (comma-separated IPs/CIDRs) and, when set, enables Fastify `trustProxy` with that **explicit list** (never blanket `true`, so only named proxies are trusted) — `request.ip`/`request.protocol` then come from `X-Forwarded-*`, fixing the audit-log client IP and the `secure` cookie behind TLS termination. Unset by default (the webui terminates TLS itself), which avoids trusting spoofable `X-Forwarded-For`. Documented in `example.env` + `docker-compose.yaml`.

## Operations

- [x] **Graceful shutdown.** Done: `src/index.ts` traps `SIGTERM`/`SIGINT` → `app.close()` (runs Fastify `onClose` hooks, which clear the session-sweep + purge timers) → `closeDb()`, then `process.exit`. A `shuttingDown` guard makes a repeated signal idempotent.
- [x] **Health endpoint.** Done: unauthenticated `GET /health` registered at the root scope in `src/app.ts` (outside the auth gate and the audit-log hook), returning `{ status: "ok" }`. Deliberately liveness-only (no DB ping) so a DB blip doesn't get the process killed; a DB-pinging readiness probe could be added separately if needed.
- [x] **Request-log retention + write-error visibility.** Done (retention + clearer errors): `purgeOldLogs(retentionDays)` in `src/middleware/logger.ts` deletes `Logs` rows older than `LOG_RETENTION_DAYS` (default 30; 0 disables), scheduled from `src/index.ts` — once at boot, then every 6h (unref'd, cleared on shutdown, kept out of `build()` so `app.inject()` tests don't hit the DB). The fire-and-forget insert's `catch` now logs with context (`"audit log write failed: …"`) instead of the bare error. Fastify pino is on whenever `NODE_ENV != "production"`; prod stays `logger:false` + DB audit trail. Audit writes remain **best-effort by design** (an audit failure must not break the request) — not retried/surfaced — and there is still **no sampling** (we already skip `/favicon.ico` + `GET /api/*` noise, so volume is low).

## Testing / DX

- [x] **Route tests via `app.inject()`.** Done: `src/app.test.ts` builds the app and injects requests (no live server) — covers the `/health` liveness route, the auth gate (unauth browser → 302 `/login`, unauth JSON `/api/*` → 401), the proxy path remaps (`/api/queue` → `/api/transaction`, `/api/hashlookups` → `/api/hashlookup`, with the `q` param forwarded), malformed-search → 400 (no upstream call), the logservice error mapping (non-2xx → 502, network/throw → 504), and the `setErrorHandler` (HTML page for browsers, JSON for `/api`/`Accept: json`). `global.fetch` is stubbed (asserts the remapped URL) and the audit-log `db.insert` is neutralized, so no DB/network is touched. Auth uses a real signed cookie via `app.signCookie`. Lives in `src/` (so it's typechecked + linted, unlike the older ad-hoc `tests/*.ts` unit tests, which remain and add X-API-Key coverage).

## Features (carried over from webui-express, still open)

- [x] User management — **done**. First-run web setup (`/setup`, gated on `countUsers() === 0`, `/login` funnels to it) creates the first admin. Full CRUD UI now lives at `/users` (secured scope): `CtrlUser` + `src/routes/users.ts` render a list (ID/email/created) with a **Create** toolbar button and per-row **edit/delete** actions (`templates/pug/users/{index,form,delete}.pug`, modeled on the relay-group pages; linked from the Config nav dropdown + dashboard). Create/edit validated by `UserCreate`/`UserEdit` (`src/validation/login.ts`); edit is "leave blank to keep password"; delete **refuses the last remaining user** (avoids lockout / re-arming `/setup`). Data helpers `listUsers`/`getUser`/`updateUser`/`deleteUser` in `src/auth/users.ts` (CLI `create_user.ts` still shares `createUser()`). Covered by 7 `app.inject()` tests. Residual: no roles/permissions — any logged-in user can manage users (see the roles item below).
- [x] `/profile` — implemented. Moved into the secured (checkSession-gated) scope in `src/app.ts`; `GET /profile` shows the logged-in user's email and a change-password form, `POST /profile` re-authenticates with the current password (`checkAuth`) before writing a new bcrypt hash (`updatePassword` in `src/auth/users.ts`). Current user is resolved via the shared `sessionEmail()` helper (`src/auth/session.ts`, now also used by `checkSession`). Form validated by `ChangePassword` (`src/validation/login.ts`); template `templates/pug/forms/profile.pug` extends `page.pug`. Covered by `app.inject()` tests (auth gate, wrong/correct current password, mismatch → no write).
- [ ] `/config/routing` — renders `util/notimpl`; routing rules are not editable from the UI.
- [ ] Roles / per-user log scoping — `User` model has only `email` + `hash`.
