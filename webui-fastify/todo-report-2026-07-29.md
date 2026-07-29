# webui-fastify TODO — audit report (2026-07-29)

Re-verification of `webui-fastify/TODO.md` against the current source. The
previous reconciliation was 2026-06-16; six commits have landed since — notably
`339e18a new grid` (w2ui → AG Grid) and `2de107c webui refresh` — so parts of the
backlog described code that no longer exists.

This pass changed **documentation only**. No source files were modified.

## Counts (current)

| Status | Count |
|---|---|
| `[x]` done | 17 |
| `[~]` partial / stubbed | 1 |
| `[ ]` not started | 16 |
| **Open work remaining** (`[ ]` + `[~]`) | **17** |

The jump from "2 open" to 17 is not regression — it is one item reclassified,
eleven previously-untracked issues added, and a five-stage epic written down. The
done column is unchanged in substance (18 → 17 `[x]` + 1 `[~]`).

---

## The finding that reframes the backlog

`Relays` / `RelayGroups` (created by `logservice/migrations/{009,010}`, edited by
`src/controllers/CtrlRelay*.ts`) have **no consumer anywhere in the stack**:

- `mailgw/plugins/npRoute.js:84-86` loads `relays.json` / `routing.json` from disk via `this.config.get` at `register`.
- Nothing under `mailgw/` opens a DB connection (no `mysql` / `drizzle` / `Bun.SQL` in `mailgw/plugins` or its `package.json`).
- `docker-compose.yaml` mounts only `./mailgw/plugins` into the mailgw container — no shared config volume exists.

So relay CRUD in the web UI is a write-only sink with zero effect on mail flow;
`/config/routing` is a stub partly because no routes table exists at all
(migrations stop at 014, and `Configs` from 011 is unused by every service); and
`auth_pass` encryption has no decrypting consumer to design key handling around.
`TODO.md` now carries this as a named section plus a five-stage epic
(`Routes` table → `/config/routing` CRUD → `GET /api/config/routing` on logservice
→ `npRoute` pull with file fallback → `auth_pass` encryption).

Design constraints recorded with the epic: `RelayGroups.name` is the top-level
key of `relays.json`; `Relays.host` maps to JSON `exchange`; `routing.json`'s
`relay` names a *group*, not a relay row; and **rule order is load-bearing** —
`RoutingTable.findRoute` returns the first match (`mailgw/plugins/RoutingTable.js:22`),
so the routes table needs an explicit `position` column and reordering in the UI.

## Corrections to existing items

| Item | Correction |
|---|---|
| `auth_pass` plaintext | Dropped "and shown in the form" — false. `templates/pug/routing/relay-form.pug:24` renders `value=''` with a "Leave blank to keep current" placeholder. Narrowed to at-rest storage and re-sequenced as stage 5 of the bridge epic. |
| Security headers residual | The residual asked whether "w2ui/jQuery" needs `'unsafe-eval'` — obsolete since the AG Grid migration. Verified it is **not** needed: no inline `script.` blocks in any pug template, grid column defs use function references rather than string expressions, and AG Grid's only two `new Function` sites are webpack's `globalThis` fallback (inside a `try`, dead in modern browsers) and `createExpressionFunction` (string-expression path, unused). Split into its own actionable item: flip `src/app.ts:92` to `reportOnly: false` and confirm a clean console. |
| Sessions | Downgraded `[x]` → `[~]`. The TTL/expiry/sweep work is real, but `src/globals.ts:9` is still a plain in-process object: every restart logs all users out and auth breaks with >1 replica. Promoted from a parenthetical footnote to the stated remaining work. |
| Route tests | Kept `[x]`, but the gap is now its own item — `tests/` is outside both `tsconfig.json` `include` and `biome.json` `files.includes`, so neither `pnpm typecheck` nor `pnpm check` sees it. It has already drifted: `tests/checkSession.test.ts:20` writes `sessions[id] = { email: 'a@b' }` with no `expiresAt` — a type error against the current `Session` interface that passes at runtime only because `undefined <= Date.now()` is `false`. |
| Snapshot | Records the AG Grid migration and notes the code comments still saying "w2ui" at `src/logservice.ts:5`, `src/routes/log.ts:5`, `src/middleware/logger.ts:14`. |

Re-verified as accurate, unchanged: proxy timeout (`AbortSignal.timeout`,
`src/logservice.ts:15,61`), `setErrorHandler`, `TRUSTED_PROXIES`/`trustProxy`,
graceful shutdown, `/health`, `purgeOldLogs`, `/login` rate limiting, search
validation, lazy DB pool, `/profile`, `/users` CRUD, first-run `/setup`.

## Newly tracked issues

**Security / correctness**
- Dead legacy assets served **outside the auth gate** (`@fastify/static` at `/`, `src/app.ts:171`): `public/lib/w2ui-1.5.{css,min.js}` (~549 KB), `public/lib/jquery-3.6.js` (282 KB, loaded by `page.pug:72` but unused — Bootstrap 5.0.2 needs no jQuery), `public/js/log-*.js`, `public/logs/*.html`. No pug template references any of them.
- `uncaughtException` handler does not exit (`src/errhandler.ts:17-21`) — the process keeps serving in an undefined state, and the `await persist()` may never land.
- Session cookie `secure: true` is unconditional (`src/auth/login.ts:32`), so plain-HTTP local dev cannot log in. Carried over from `AGENTS.md`; was never in `TODO.md`.

**Observability / DX**
- Silent failure paths: bare `catch {}` around `recentDeliveries()` (`src/routes/root.ts:29-31`), `console.error` logging the attempted email (`src/auth/util.ts:14`), unguarded cert `readFileSync` (`src/index.ts:58-59`).
- `example.env` drift: `LOGSERVICE_TIMEOUT_MS` and `LOG_RETENTION_DAYS` undocumented; `AUTH_DB_*` and `DB_DRIVER` dead.
- `package.json` `test` is a bare `node --test` (no `--no-warnings`, unlike `start`).
- No coverage for the three controllers (the whole `/config` surface), `purgeOldLogs`, session expiry, `errhandler.ts`, `validation/config.ts`, `db/`. No coverage tooling, no CI config in this package.
- `src/adapter.ts` is a vestigial 8-line re-export of uuid + bcrypt.

## Doc hygiene

- `AGENTS.md` reduced to its two live findings (unconditional `secure` cookie + in-memory sessions; no queue/backoff on audit writes) with a pointer to `TODO.md`. Its items 2, 3, 5 and 6 were done and have been removed.
- `todo-report-2026-06-16.md` deleted — superseded by this file, and it claimed "not yet committed" for work that has since shipped.
- `CLAUDE.md` notes that the relay/routing config UI does not yet feed Haraka.

## Suggested order of attack

1. **Flip CSP to enforcing** (`src/app.ts:92`) — the analysis is done, it is a one-line change plus a browser pass.
2. **Delete the dead w2ui/jQuery assets** — ~830 KB of unauthenticated stale surface, no behavioural risk.
3. **`uncaughtException` → exit** and the three silent-failure spots — small, and they are what will make the next incident diagnosable.
4. **Bring `tests/` into typecheck + lint** and fix the stale `checkSession` test.
5. **The config-bridge epic** — the only item that changes what the product actually does. Everything above is a day's work in total; this is the real project.
