Testing plan for webui-fastify

Goal: add fast, focused unit tests that catch regressions for auth gating and the logservice proxy.

P0 (first to implement)
- webui-fastify/tests/logservice.search.test.ts — stub global fetch and verify q param + X-API-Key header behavior.
- webui-fastify/tests/checkSession.test.ts — test redirect for unauthenticated and pass-through for valid session.

P1 (next)
- webui-fastify/tests/logger.test.ts — spy db.insert and verify row shape and that errors are caught.
- webui-fastify/tests/api.routes.test.ts — build app() and inject GET /api/* with mocked fetch; assert response body/type.

P2 (later)
- auth/util.test.ts — mock db select and bcrypt.compare to validate checkAuth true/false paths.
- auth/login.test.ts — verify setCookie options (secure derived from env) and that session is created.

Test patterns & notes
- Avoid importing src/index.ts in tests because it runs DB assert and exits if env missing. Import specific modules: src/logservice.ts, src/middleware/checkSession.ts, etc.
- Mock fetch: set `globalThis.fetch = async (url, opts) => ({ ok:true, status:200, json:async()=>({}) })`.
- Mutate exported objects for DB spies (e.g. import { db } and replace db.insert temporarily). Restore after test.
- Use Node's built-in test runner: package.json's `test` script runs `node --test`.

Run tests locally
- cd webui-fastify
- pnpm test
