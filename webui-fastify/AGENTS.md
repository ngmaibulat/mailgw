AGENTS: High-signal review notes for webui-fastify

Purpose: keep short, actionable guidance an automated agent would otherwise miss.

The full backlog lives in `TODO.md`; the latest audit is `todo-report-2026-07-29.md`.
Only findings still open are kept here.

Live findings
1) Session safety and UX (high)
   - Sessions are in-memory (`src/globals.ts:9`). They now carry an 8h TTL and are
     swept, but the store is still a plain in-process object: every restart or
     deploy logs all users out, and auth breaks outright with more than one replica.
   - Cookies are set with `secure: true` unconditionally (`src/auth/login.ts:32`),
     which breaks local non-TLS dev — the browser drops the cookie and every
     request bounces back to `/login`.
   - Action: make the session store pluggable (`@fastify/session` + Redis or a
     DB-backed table), and make cookie `secure` env/protocol-driven with a `true`
     default.

2) Fire-and-forget DB logging (low/medium)
   - The audit-log insert in `src/middleware/logger.ts:51` is not awaited. It now
     has a contextual `.catch`, and best-effort is deliberate (an audit failure
     must not break the request), but there is no bounded queue or backoff.
   - Action: only if audit completeness ever becomes a requirement — otherwise
     leave as-is and keep the documented rationale in `TODO.md`.

Resolved since the original notes (do not re-report)
- `bcrypt.compareSync` → async `bcrypt.compare` (`src/auth/util.ts`).
- `checkSession` returns 401 JSON for `Accept: application/json`, 302 for browsers.
- `SIGN_COOKIE` is in `checkenv.ts`'s REQUIRED list and `build()` throws without it;
  the `|| 'sign'` fallback is gone.
- logservice proxy errors map to 502 (upstream non-2xx) / 504 (network, incl. the
  `LOGSERVICE_TIMEOUT_MS` abort) instead of a blanket 500.
