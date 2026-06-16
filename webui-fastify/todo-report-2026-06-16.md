# webui-fastify TODO — status report (2026-06-16)

Snapshot of `webui-fastify/TODO.md`, updated after this session's work. Legend: `[x]` done · `[~]` partial · `[ ]` not started.

## Counts (current)

| Status | Count |
|---|---|
| `[x]` done | 18 |
| `[~]` partial / stubbed | 0 |
| `[ ]` not started | 2 |
| **Open work remaining** (`[ ]` + `[~]`) | **2** |

> At the start of the session the backlog stood at 15 open. Thirteen items were resolved this session (see below).

## Open items by section

### High — bugs & security (1 open)
- `[ ]` `auth_pass` stored plaintext — encrypt at rest (`db/schema.ts`)

### Medium — robustness (0 open) ✅ block cleared this session

### Operations (0 open) ✅ block cleared this session

### Testing / DX (0 open) ✅ cleared this session

### Features carried from webui-express (2 open)
- `[ ]` `/config/routing` — `notimpl` stub
- `[ ]` Roles / per-user log scoping

## Priority read

With the Operations block, the Medium block, and most security hardening handled, the remaining **High — bugs & security** item is the priority:
1. **`auth_pass` plaintext** — credential-at-rest exposure for relay targets; the one remaining active risk.

The Medium, Operations, and Testing/DX blocks are fully cleared, so the only non-feature work left is that single High security item. The two remaining `[ ]` beyond it are carried-over feature stubs (`/config/routing` editor, roles / per-user log scoping).

## Resolved this session

**Security / robustness**
- Session expiry + proper server-side logout — TTL store (`expiresAt`), expiry-aware `getSession()`, `deleteSession()` on logout, periodic `sweepSessions()`.
- Proxy fetch timeout — `AbortSignal.timeout` (`LOGSERVICE_TIMEOUT_MS`, default 10s) → maps to 504 on a hung logservice.
- Fastify `setErrorHandler` — renders `util/error.pug` (generic 5xx, real-message 4xx); JSON for `/api/*` + `Accept: application/json`.
- Removed the misleading `SIGN_COOKIE || "sign"` fallback — `build()` now throws if unset (presence was already enforced by `checkenv`), so no path runs on a hardcoded secret.
- `trustProxy` made config-driven — `TRUSTED_PROXIES` (comma-separated IPs/CIDRs) enables Fastify `trustProxy` with an explicit list when set; unset by default. Documented in `example.env` + `docker-compose.yaml`.
- Security headers via `@fastify/helmet` — enforced HSTS/X-Frame-Options/nosniff/Referrer-Policy/COOP/CORP on every response; CSP tailored to the UI but shipped report-only (flip to enforce after browser verification). Header test added.
- Brute-force protection on `/login` — `@fastify/rate-limit` registered `global: false`; `POST /login` opts in via `config.rateLimit` (`LOGIN_RATE_MAX` / `LOGIN_RATE_WINDOW`, default 5 / "1 minute", keyed per `request.ip` — correct behind `trustProxy`). The plugin throws a 429 that lands in the existing `setErrorHandler`: browser form posts redirect to `/login?msg=TooManyAttempts` (styled alert via `public/js/login.js`, which now maps `msg` → message text), JSON/API clients keep the raw 429. Env knobs documented in `example.env`; `app.inject()` test added.
- Route tests via `app.inject()` — `src/app.test.ts` (now 34 tests): `/health`, auth gate (302/401), proxy path remaps + `q` forwarding, 400 on malformed search, 502/504 upstream mapping, the HTML/JSON error handler, login rate limiting, the first-run setup flow, the /profile change-password flow, and the /users CRUD (list/create/edit/delete + last-user guard). `fetch` + audit `db.insert`/`select`/`update`/`delete`/`$count` stubbed; no DB/network touched.

**Features**
- First-run setup (not on the original backlog) — startup logs a hint when no users exist; unauthenticated `/setup` creates the first admin, gated on `countUsers() === 0` (both GET and POST refuse once any user exists, so it's not an open registration endpoint). `/login` funnels to `/setup` on first run. `create_user.ts` CLI refactored to share `createUser()` (`src/auth/users.ts`).
- `/profile` (was a `notimpl` stub) — moved into the secured/checkSession scope; `GET` shows the logged-in user's email + a change-password form, `POST` re-authenticates with the current password (`checkAuth`) before writing a new bcrypt hash (`updatePassword`). Current user resolved via a new shared `sessionEmail()` helper (`src/auth/session.ts`, also adopted by `checkSession`). Validated by `ChangePassword` (`src/validation/login.ts`); template extends `page.pug`. Five `app.inject()` tests added.
- User management UI (closes the carried-over feature) — full CRUD at `/users` (secured scope): `CtrlUser` + `src/routes/users.ts` render a user list (ID/email/created) with a **Create** toolbar button and per-row **edit/delete** actions (`templates/pug/users/{index,form,delete}.pug`, modeled on the relay-group pages; linked from the Config nav dropdown + dashboard). Validated by `UserCreate`/`UserEdit`; edit is "leave blank to keep password"; delete **refuses the last remaining user** (avoids lockout / re-arming `/setup`). New data helpers `listUsers`/`getUser`/`updateUser`/`deleteUser` (`src/auth/users.ts`). Seven `app.inject()` tests added. Residual: no roles — any logged-in user can manage users (tracked separately).

**Operations block (all three)**
- Graceful shutdown — `SIGTERM`/`SIGINT` → `app.close()` (clears timers via `onClose`) → `closeDb()`.
- Health endpoint — unauthenticated `GET /health` (liveness-only, no DB ping), outside the auth gate + audit log.
- Request-log retention + clearer write errors — `purgeOldLogs()` deletes `Logs` rows older than `LOG_RETENTION_DAYS` (default 30; 0 disables), scheduled at boot + every 6h; insert `catch` now logs with context. Audit writes stay best-effort by design; no sampling added (noise already filtered, retention now bounds size).

All changes pass `pnpm typecheck` + `pnpm check` (Biome). Working-tree only — not yet committed.
