# webui-fastify TODO

Improvement backlog for the active admin UI (Fastify + Drizzle, pug-only,
native HTTP/2, **TypeScript**). Items derived from reading the current code;
file refs are where the work lands.

**Legend:** `[x]` done · `[~]` partial / stubbed · `[ ]` not started

Last reconciled against source: **2026-07-29** (see `todo-report-2026-07-29.md`).

---

## At a glance — 17 open (16 `[ ]` + 1 `[~]`), 17 done

| # | Open item | Where | Size |
|---|---|---|---|
| 1 | Flip CSP report-only → enforcing (analysis done, `'unsafe-eval'` not needed) | `src/app.ts:92` | XS |
| 2 | Delete dead w2ui/jQuery assets (~830 KB, served outside the auth gate) | `public/lib`, `public/js/log-*.js`, `public/logs` | XS |
| 3 | `uncaughtException` logs but never exits — process keeps serving broken | `src/errhandler.ts:17` | XS |
| 4 | Silent failures: bare `catch {}`, `console.error` w/ email, unguarded cert read | `routes/root.ts:29`, `auth/util.ts:14`, `index.ts:58` | S |
| 5 | `example.env` drift — 2 vars undocumented, `AUTH_DB_*`/`DB_DRIVER` dead | `example.env` | XS |
| 6 | Cookie `secure: true` unconditional — blocks plain-HTTP local dev | `src/auth/login.ts:32` | XS |
| 7 | Delete vestigial uuid/bcrypt re-export | `src/adapter.ts` | XS |
| 8 | `tests/` outside typecheck **and** lint; one test already stale | `tsconfig.json`, `biome.json`, `tests/checkSession.test.ts:20` | S |
| 9 | No coverage for controllers, `purgeOldLogs`, session expiry, `errhandler`, `db/` | — | M |
| 10 | `[~]` Sessions in-memory — restart logs everyone out, breaks with >1 replica | `src/globals.ts:9` | M |
| 11 | Roles / per-user scoping — any logged-in user can manage users + config | `db/schema.ts` | M |
| 12–16 | **Config-bridge epic** — `Routes` table → `/config/routing` CRUD → logservice `GET /api/config/routing` → `npRoute` pull w/ file fallback → `auth_pass` encryption | cross-service | L |

Items 1–5 are the cheap, high-value pass. Item 12–16 is the only work that
changes what the product does: **today the relay config UI does not affect mail
flow at all** (see the next section). `auth_pass` encryption is stage 5 of that
epic, not a standalone task — until stage 3 there is no consumer to decrypt it.

---

## Snapshot — what exists today

- **TypeScript** throughout (`.ts`), run directly by Node 26 via native type-stripping — no build step / no emit. `tsconfig.json` (`verbatimModuleSyntax` + `erasableSyntaxOnly`) is for `pnpm typecheck` + editor only; relative imports carry the real `.ts` extension. `typescript`/`@types/*` are devDeps, omitted from the prod image.
- **Biome** lint + format (`biome.json`, recommended rules, style matched to the old prettier/editorconfig). Scoped to `src`/`db`/root `*.ts`+`*.json`; `public/lib` (vendored) and `public/js` (browser scripts) are excluded; import-sort assist off (keeps manual import groups). Scripts: `lint`, `format`, `check`, `check:fix`. CI should gate on `pnpm typecheck` + `pnpm check` (type-stripping does not type-check at runtime). **Note:** `tests/` is outside both scopes — see the testing section.
- Native HTTP/2 server (`src/index.ts`, Fastify `{ http2, https: { allowHTTP1 } }`); requires `./certs/server.{key,crt}`.
- Session login (`/login`, bcrypt vs `User.hash`), **in-memory** session store with an 8h TTL, `checkSession` preHandler guard.
- Encapsulated auth scopes in `src/app.ts` (public static → logged → secured).
- Log viewer pages (pug): connection, delivery, mails, lookups — rendered client-side with **AG Grid Community** (`public/lib/aggrid/`, `public/js/grids/*.js`) since commit `339e18a`. The older w2ui implementation is gone from the templates but its assets are still shipped (see the dead-assets item below).
- Read-only `/api/{connection,delivery,queue,hashlookups}` — **proxied to logservice** (`src/logservice.ts`); no ingest API, no `/filter/md5`.
- Relay & relay-group CRUD under `/config` (`CtrlRelay`, `CtrlRelayGroup`), validated with drizzle-zod. **These rows are not consumed by anything — see "DB → Haraka config bridge" below.**
- User CRUD at `/users`, first-run `/setup`, `/profile` change-password.
- **Drizzle ORM** (`db/schema.ts` + `db/index.ts`), scoped to what the webui owns: `users`, `relays`, `relayGroups`, `logs`, `exceptions`. The webui does not own/migrate the schema (logservice does).
- CLI user tools: `create_user.ts`, `check_user.ts`.

---

## The big picture — the config UI is disconnected from the gateway

`Relays` / `RelayGroups` (created by `logservice/migrations/{009,010}`, edited by
`src/controllers/CtrlRelay.ts` / `CtrlRelayGroup.ts`) have **no consumer anywhere
in the stack**. Haraka routes purely from files on disk — `mailgw/plugins/npRoute.js:84-86`
reads `relays.json` / `routing.json` through `this.config.get` at `register`, and
nothing under `mailgw/` opens a DB connection. `docker-compose.yaml` mounts only
`./mailgw/plugins` into the mailgw container, so there is not even a shared config
volume.

Three backlog items follow directly from this, and they are sequenced as one epic
at the bottom of this file:

- Editing a relay in the web UI currently has **zero effect on mail flow** — it is a write-only sink.
- `/config/routing` is a stub partly because **there is no routes table at all** (migrations stop at 014; `Configs` from 011 is unused by every service).
- `auth_pass` encryption-at-rest has **no decrypting consumer** to design key handling around, so it is sequenced *after* the bridge rather than before it.

---

## High priority — bugs & security

- [x] **Relay edit wipes `auth_pass` (data loss).** Fixed in the Drizzle migration: `CtrlRelay.editHandle` drops an empty `auth_pass` from the update set ("leave blank to keep"), and the form shows a placeholder hint.
- [~] **Sessions never expire and logout is weak.** *Expiry* is fixed: `src/globals.ts` stores `{ email, expiresAt }` (TTL `SESSION_TTL_MS` = 8h, synced with the login cookie's `maxAge`); `getSession()` treats expired entries as absent and prunes on access; `src/auth/logout.ts` unsigns the cookie and `deleteSession()`s the server-side entry; `src/app.ts` runs an unref'd `sweepSessions()` interval (cleared on close). **Still open:** the store is a plain in-process object (`src/globals.ts:9`), so every restart/deploy logs all users out and auth breaks outright with more than one replica. Moving to `@fastify/session` + a real store (Redis, or a DB-backed table) is the remaining work, not an optional footnote.
- [x] **No validation on config mutations (mass-assignment).** Fixed: `src/validation/config.ts` derives drizzle-zod insert schemas (`.pick()` to form fields → strips extras; `.extend()` requires `name`/`host`); controllers `safeParse` and re-render the form with an error on failure.
- [ ] **`auth_pass` stored plaintext** (`db/schema.ts:49`, `relays.auth_pass` `varchar`). At-rest only — the edit form does *not* echo the stored value back (`templates/pug/routing/relay-form.pug:24` renders `value=''` with a "Leave blank to keep current" placeholder). Encrypting is blocked on there being a consumer that decrypts: **do this as step 5 of the config-bridge epic**, not standalone.
- [ ] **Session cookie `secure: true` is unconditional** (`src/auth/login.ts:32`). Correct for the deployed setup (the webui terminates TLS itself), but it makes plain-HTTP local dev unable to log in at all — the browser drops the cookie and every request bounces back to `/login`. Make it env/protocol-driven (`NODE_ENV`, `request.protocol`, or an explicit `COOKIE_SECURE`), defaulting to `true`.
- [x] **Weak cookie secret default.** `SIGN_COOKIE` is in `src/checkenv.ts`'s `REQUIRED` list, so the server refuses to boot without it, and `build()` throws if it is unset so non-checkenv paths (e.g. tests) can't run on a hardcoded secret either. Residual (minor): no *strength*/min-length check on the value, and `example.env` ships a throwaway sample that is easy to copy verbatim.
- [x] **No brute-force protection on `/login`.** `@fastify/rate-limit` registered in `src/app.ts` with `global: false`; `POST /login` opts in via `config.rateLimit` (`LOGIN_RATE_MAX` req / `LOGIN_RATE_WINDOW`, default 5 / "1 minute", keyed per `request.ip` — correct behind `trustProxy`). On exceed the plugin throws a 429 that lands in the existing `setErrorHandler`: browser form posts are redirected to `/login?msg=TooManyAttempts` (styled alert via `public/js/login.js`), JSON/API clients keep the 429. Covered by an `app.inject()` test.
- [x] **Security headers.** `@fastify/helmet` registered at the root in `src/app.ts`, so every response (static, `/health`, all routes) gets HSTS, `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, COOP/CORP, etc. **enforced**. Covered by an `app.inject()` header test.
- [ ] **Flip CSP from report-only to enforcing** (`src/app.ts:92`, `reportOnly: true`). The policy is already tailored to this UI (`script-src 'self'`, `style-src 'self' 'unsafe-inline'`, `img-src 'self' data:`, `object-src 'none'`, `frame-ancestors 'none'`). The old blocker — "does the grid library need `'unsafe-eval'`?" — was written against w2ui/jQuery and no longer applies: with AG Grid there are no inline `script.` blocks in any pug template, the grid column defs in `public/js/grids/*.js` use function references rather than string expressions, and AG Grid's only two `new Function` call sites are webpack's `globalThis` fallback (inside a `try`, dead in any modern browser) and `createExpressionFunction` (the string-expression path, unused here). **Action:** set `reportOnly: false`, load each of the four grid pages plus the config forms, confirm a clean console.

## Medium — robustness

- [x] **Search/query requests are forwarded unvalidated.** Fixed: `src/validation/search.ts` defines a zod v4 `searchRequest` schema mirroring logservice's accepted shape (`{ search: [{ field, operator, value }], searchLogic, limit, offset }`, operators + `AND`/`OR` enumerated, `value` scalar-or-2-tuple, non-negative int `limit`/`offset`). `parseSearchRequest` decodes the `?request=<json>` string, `safeParse`s it, and returns either the re-serialized (unknown-keys-stripped) JSON to forward or an error; `src/routes/api.ts`'s `proxySearch` returns **400** on failure instead of bouncing a bad request off logservice. logservice still does the authoritative field whitelist + parse (defense-in-depth).
- [x] **Proxy timeout.** `src/logservice.ts:61` passes `signal: AbortSignal.timeout(TIMEOUT_MS)` (env `LOGSERVICE_TIMEOUT_MS`, default 10s) to the upstream `fetch`; a timeout surfaces as a `LogserviceError` of `kind: "network"` → **504** (with a "timed out after Nms" reason), so a hung logservice no longer hangs the webui request. Could still optionally return a friendly empty grid payload instead of an error.
- [x] **Stale schema-check magic number.** Gone with the Sequelize removal — the brittle `< 15` table-count check no longer exists (the webui doesn't inspect/own the schema).
- [x] **DB connect + `process.exit` at import time.** Fixed: `db/index.ts` is a lazy `mysql2` pool (no import-time connect); `src/index.ts` calls `assertDbConnection()` (a ping) at startup and exits only there on failure.
- [x] **`setErrorHandler`.** Added in `src/app.ts`: handler exceptions render `util/error.pug` (a friendly HTML page, generic message for 5xx so internals aren't leaked, the real message for 4xx) instead of Fastify's bare-JSON 500. API/JSON clients (`Accept: application/json` or `/api/*`) still get `{ status, message }` JSON. The process-level `uncaughtException`/`unhandledRejection` handlers in `src/errhandler.ts` remain for top-level safety.
- [x] **`trustProxy`.** Config-driven: `src/app.ts` reads `TRUSTED_PROXIES` (comma-separated IPs/CIDRs) and, when set, enables Fastify `trustProxy` with that **explicit list** (never blanket `true`) — `request.ip`/`request.protocol` then come from `X-Forwarded-*`, fixing the audit-log client IP and the `secure` cookie behind TLS termination. Unset by default (the webui terminates TLS itself), which avoids trusting spoofable `X-Forwarded-For`. Documented in `example.env` + `docker-compose.yaml`.
- [ ] **`uncaughtException` handler does not exit** (`src/errhandler.ts:17-21`). It logs and persists to `Exceptions`, then lets the process keep serving in an undefined state — the one thing an uncaught-exception handler must not do. The `await persist(...)` is also inside an async listener, so on a crash-adjacent path the write may never land. Fix: persist best-effort behind a short timeout, then `process.exit(1)` and let the container restart.
- [ ] **Dead legacy assets served unauthenticated.** `@fastify/static` mounts `public/` at `/` (`src/app.ts:171`) *outside* the auth gate, and these are all still shipped and publicly reachable despite no pug template referencing them: `public/lib/w2ui-1.5.{css,min.js}` (~549 KB), `public/lib/jquery-3.6.js` (282 KB — loaded by `templates/pug/page.pug:72` but unused, Bootstrap 5.0.2 needs no jQuery), `public/js/log-{connection,delivery,lookups,mails}.js` (the old w2ui grid drivers), and `public/logs/*.html` (standalone w2ui pages). They return no data unauthenticated (their `/api/*` calls 401), but they are ~830 KB of stale attack surface. Delete them and drop the jQuery `<script>` from `page.pug`; re-grep for `$(` / `jQuery` across `public/js` + `templates` first.

## Operations

- [x] **Graceful shutdown.** `src/index.ts` traps `SIGTERM`/`SIGINT` → `app.close()` (runs Fastify `onClose` hooks, which clear the session-sweep + purge timers) → `closeDb()`, then `process.exit`. A `shuttingDown` guard makes a repeated signal idempotent.
- [x] **Health endpoint.** Unauthenticated `GET /health` registered at the root scope in `src/app.ts` (outside the auth gate and the audit-log hook), returning `{ status: "ok" }`. Deliberately liveness-only (no DB ping) so a DB blip doesn't get the process killed; a DB-pinging readiness probe could be added separately if needed.
- [x] **Request-log retention + write-error visibility.** `purgeOldLogs(retentionDays)` in `src/middleware/logger.ts` deletes `Logs` rows older than `LOG_RETENTION_DAYS` (default 30; 0 disables), scheduled from `src/index.ts` — once at boot, then every 6h (unref'd, cleared on shutdown, kept out of `build()` so `app.inject()` tests don't hit the DB). The fire-and-forget insert's `catch` logs with context (`"audit log write failed: …"`). Fastify pino is on whenever `NODE_ENV != "production"`; prod stays `logger:false` + DB audit trail. Audit writes remain **best-effort by design** (an audit failure must not break the request) — not retried/surfaced — and there is still **no sampling** (we already skip `/favicon.ico` + `GET /api/*` noise, so volume is low).
- [ ] **Silent failure paths.** Three spots swallow or misroute diagnostics: `src/routes/root.ts:29-31` catches *every* `recentDeliveries()` error with a bare `catch {}` and no `request.log`, so a persistently broken logservice is invisible from the dashboard path; `src/auth/util.ts:14` uses `console.error` (bypassing pino) and logs the attempted email address; `src/index.ts:58-59` reads the TLS certs with an unguarded `readFileSync`, producing a raw ENOENT stack instead of the friendly `checkenv`-style message the certs deserve as a hard boot requirement.
- [ ] **Env documentation drift** (`example.env`). `LOGSERVICE_TIMEOUT_MS` and `LOG_RETENTION_DAYS` are read by the code but undocumented; the `AUTH_DB_*` block and `DB_DRIVER` are dead (no code reads them). Reconcile against the full list: `DB_HOST`, `DB_USER`, `DB_PASS`, `DB_NAME`, `SIGN_COOKIE` (required); `PORT`, `NODE_ENV`, `LOG_LEVEL`, `LOG_RETENTION_DAYS`, `TEMPLATE_DIR`, `TRUSTED_PROXIES`, `LOGIN_RATE_MAX`, `LOGIN_RATE_WINDOW`, `LOGSERVICE_URL`, `LOGSERVICE_API_KEY`, `LOGSERVICE_TIMEOUT_MS` (optional).

## Testing / DX

- [x] **Route tests via `app.inject()`.** `src/app.test.ts` builds the app and injects requests (no live server) — covers `/health`, the auth gate (unauth browser → 302 `/login`, unauth JSON `/api/*` → 401), the proxy path remaps (`/api/queue` → `/api/transaction`, `/api/hashlookups` → `/api/hashlookup`, with the `q` param forwarded), malformed-search → 400 (no upstream call), the logservice error mapping (non-2xx → 502, network/throw → 504), the `setErrorHandler`, login rate limiting, the first-run setup flow, `/profile`, and `/users` CRUD. `global.fetch` is stubbed (asserts the remapped URL) and the audit-log `db` calls are neutralized, so no DB/network is touched. Auth uses a real signed cookie via `app.signCookie`.
- [ ] **`tests/` is neither typechecked nor linted.** The older ad-hoc suite (`tests/api.routes.test.ts`, `checkSession.test.ts`, `logservice.search.test.ts`) sits outside `tsconfig.json`'s `include` **and** `biome.json`'s `files.includes`, so `pnpm typecheck` and `pnpm check` both skip it — and it has already drifted: `tests/checkSession.test.ts:20` writes `sessions[id] = { email: 'a@b' }` with no `expiresAt`, which is a type error against the current `Session` interface and passes at runtime only by accident (`undefined <= Date.now()` is `false`, so the entry reads as un-expired). Either fold these into `src/*.test.ts` or add `tests/**` to both configs and fix the fallout. Related: `package.json`'s `test` script is a bare `node --test` without the `--no-warnings` the `start` script uses.
- [ ] **Untested areas.** No coverage at all for: the three controllers (relay/relay-group CRUD in particular — the entire `/config` surface), `purgeOldLogs`, session expiry / `sweepSessions`, `errhandler.ts`, `validation/config.ts`, and `db/`. No coverage tooling and no CI config in this package.
- [ ] **Vestigial `src/adapter.ts`.** Eight lines re-exporting `uuidv4` + `bcrypt`, left over from the pre-Drizzle era. Import those directly at the two call sites and delete the file.

## Features (carried over from webui-express)

- [x] User management — full CRUD at `/users` (secured scope): `CtrlUser` + `src/routes/users.ts` render a list (ID/email/created) with a **Create** toolbar button and per-row **edit/delete** actions (`templates/pug/users/{index,form,delete}.pug`, linked from the Config nav dropdown + dashboard). First-run web setup (`/setup`, gated on `countUsers() === 0`, `/login` funnels to it) creates the first admin. Create/edit validated by `UserCreate`/`UserEdit` (`src/validation/login.ts`); edit is "leave blank to keep password"; delete **refuses the last remaining user** (avoids lockout / re-arming `/setup`). Data helpers `listUsers`/`getUser`/`updateUser`/`deleteUser` in `src/auth/users.ts` (CLI `create_user.ts` shares `createUser()`). Covered by 7 `app.inject()` tests.
- [x] `/profile` — `GET` shows the logged-in user's email and a change-password form; `POST` re-authenticates with the current password (`checkAuth`) before writing a new bcrypt hash (`updatePassword` in `src/auth/users.ts`). Lives in the secured scope; current user resolved via the shared `sessionEmail()` helper (`src/auth/session.ts`, also used by `checkSession`). Validated by `ChangePassword` (`src/validation/login.ts`). Covered by `app.inject()` tests.
- [ ] **Roles / per-user log scoping.** The `users` table has only `email` + `hash`, so any logged-in user can manage users, relays, and (once built) routing. Needs at minimum an admin/viewer split before this UI is exposed to more than one operator.

---

## Epic — DB → Haraka config bridge

Closes the disconnect described at the top of this file: make the relay/routing
rows the UI edits actually drive Haraka, and give `/config/routing` something to
edit. **Recommended transport: an HTTP pull from logservice** — it already owns
the migrations and the DB, mailgw already speaks HTTP to it (`npData`, `npQueue`,
`npLogDelivery`), and it needs neither a shared volume nor a DB client inside
Haraka.

Shape mismatches to respect when mapping rows → Haraka JSON:

- `RelayGroups.name` is the **top-level key** of `relays.json` (`"Exchange"`, `"Outbound"`), and its value is an **array** of relay objects.
- `Relays.host` → JSON `exchange`; `Relays.port` is an INT column while the JSON uses a string.
- `routing.json`'s `relay` field names a **relay group**, not an individual relay row.
- **Rule order is load-bearing.** `RoutingTable.findRoute` returns the *first* matching route (`mailgw/plugins/RoutingTable.js:22`), so a routes table needs an explicit `position` column and the UI needs reordering controls. The current config model has no notion of this.

Stages:

- [ ] **1. `Routes` table.** New `logservice/migrations/015_create_routes.sql`: `id`, `routename`, `sender`, `sender_domain`, `rcpt`, `rcpt_domain`, `relay_group_id` (→ `RelayGroups.id`), `position`, `createdAt`, `updatedAt`. Mirror it in `db/schema.ts` as a describe-only table, per that file's existing no-DDL rule.
- [ ] **2. `/config/routing` CRUD** — replaces the `notimpl` stub at `src/routes/config-relay.ts:37-39`. Model on `CtrlRelayGroup` + `templates/pug/routing/relaygrp-*.pug`; validate with drizzle-zod through `src/validation/config.ts`; add move-up / move-down actions for `position`.
- [ ] **3. `GET /api/config/routing` on logservice** — assembles `{ relays: {...}, routes: [...] }` already in Haraka's shapes (group → array, `host` → `exchange`, port → number, routes ordered by `position`). Wrap in the existing `handle()` = `withAuth(withErrorHandling(...))` so `X-API-Key` gates it.
- [ ] **4. `npRoute` pull + fallback** — fetch at `register` (URL from `mailgw/config/logging.json`, e.g. `url_routing`), **falling back to the existing `config.get()` files** when unset or on failure, so local dev and today's deployment keep working unchanged. Refresh on an interval and atomically swap the module-level `rtable` (`hook_get_mx` reads it per call, so a whole-object swap is safe); never replace a good table with a failed fetch.
- [ ] **5. `auth_pass` encryption at rest** — now has a real consumer. AES-256-GCM via `node:crypto` under a `CONFIG_SECRET_KEY` shared by the webui (encrypt on write) and logservice (decrypt on serve). Sequence after stage 3.
