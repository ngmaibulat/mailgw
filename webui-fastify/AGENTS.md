AGENTS: High-signal review notes for webui-fastify

Purpose: keep short, actionable guidance an automated agent would otherwise miss.

Top findings (prioritised)
1) Authentication / session safety and UX (high)
   - Sessions are in-memory (src/globals.ts). Cookies are set with `secure: true` unconditionally (src/auth/login.ts). This breaks local non-TLS dev and is unsafe for multi-instance deployments (sessions lost on restart).
   - Action: make session store pluggable (Redis) or document single-instance only; make cookie `secure` configurable via env.

2) Blocking bcrypt call (medium-high)
   - Code uses `bcrypt.compareSync` in async path (src/auth/util.ts) which blocks the event loop on expensive hashes.
   - Action: use the async `bcrypt.compare` (or bcryptjs Promise wrapper) instead.

3) API vs browser auth behaviour (medium)
   - checkSession always redirects to `/login` (src/middleware/checkSession.ts). API/fetch clients should receive 401 JSON.
   - Action: detect `Accept: application/json` or `X-Requested-With` and return 401 JSON for programmatic clients; keep redirect for browser navigation.

4) Fire-and-forget DB logging (medium)
   - Logger does a non-awaited insert with `Promise.resolve(...).catch(console.error)` (src/middleware/logger.ts). Errors are only console.logged and insertion is best-effort.
   - Action: handle promise explicitly, attach `.catch(...)`, or use a bounded queue/backoff for production reliability.

5) Env and secret validation (medium)
   - checkenv.ts validates DB_* but not SIGN_COOKIE; app uses a weak default `SIGN_COOKIE || 'sign'` (src/app.ts).
   - Action: include SIGN_COOKIE in required envs or fail-fast when missing.

6) logservice proxy error handling (low/medium)
   - Upstream errors from logservice are thrown and become 500s; status/body are not forwarded (src/routes/api.ts, src/logservice.ts).
   - Action: wrap proxy calls, map upstream statuses to 502/504, or safely forward upstream JSON/status for easier debugging.

Developer-suggested quick fixes (minimal, safe)
- Replace `bcrypt.compareSync` with `await bcrypt.compare(...)` in src/auth/util.ts (non-breaking, small).
- Make cookie `secure` controlled by env (e.g. COOKIE_SECURE or NODE_ENV check) in src/auth/login.ts (small).
- Return 401 JSON for API requests in src/middleware/checkSession.ts when Accept includes `application/json` (small).
- Add SIGN_COOKIE to checkenv.ts REQUIRED list so startup fails with clear message if unset (small).

Tests and follow-ups
- Add unit tests for auth/login cookie flags, checkSession JSON vs redirect, and logservice proxy error handling.
- If moving to a shared session store, add migration docs and an env example in README/CLAUDE.md.

If you want, I can apply the four quick fixes now and run a typecheck. Reply with "apply fixes" to proceed or tell me which subset to implement.
