todo-logging: webui-fastify logging improvements
Date: 2026-06-16T14:23:56+03:00

Summary — prioritized, actionable logging improvements (review only; no code changes applied):

1) High — adopt structured logging
- Integrate a structured logger (pino) through Fastify's logger option instead of console.*.
- Replace ad-hoc console.log/console.error with logger.info/warn/error and use JSON output for easy ingestion by log aggregators.

2) High — record full request lifecycle
- Generate and attach an X-Request-Id to every request (onRequest).
- Log request start (onRequest) and completion (onResponse) with status, latency, method, path, src_ip, user/session id, and request-id.

3) High — make DB request logging reliable
- Avoid silent fire-and-forget DB inserts in middleware/logger.ts.
- Use an explicit, bounded background queue (in-process) with retries/backoff for log writes or await inserts with error handling and metrics.
- Ensure logging failures do not crash the app but are surfaced to monitoring.

4) High — improve uncaught error handling
- Capture both uncaughtException and unhandledRejection with structured logs containing timestamp, stack, host/pid, and a request-id when available.
- Persist exception records robustly (with retries) and emit an alertable log level (error/critical).

5) Medium — redact sensitive data and add log levels
- Implement a sanitization step before persisting or forwarding logs to remove PII (emails, auth tokens, passwords).
- Allow LOG_LEVEL via env (default INFO) and use it to control what gets persisted.

6) Medium — robust logservice proxy logging
- Wrap logservice proxy calls: on non-2xx, capture upstream status/body and map to 502/504 with context, or forward upstream JSON/status safely for debugging.
- Log latency and upstream request-id if provided.

7) Low — structured sinks, rotation, and docs
- Configure logger sinks (stdout for containers, file with rotation for local) and optional external sink (filebeat/HTTP).
- Document logging-related env vars (LOG_LEVEL, LOG_FORMAT, COOKIE_SECURE, SIGN_COOKIE) in example.env and AGENTS.md.

Quick developer-suggested safe fixes (non-breaking):
- Replace bcrypt.compareSync with async bcrypt.compare in src/auth/util.ts.
- Make cookie secure flag configurable via env in src/auth/login.ts.
- Return 401 JSON for API clients in src/middleware/checkSession.ts when Accept: application/json.
- Add SIGN_COOKIE to checkenv.ts REQUIRED so startup fails clearly when missing.

Next steps
- Propose precise code edits and tests for the items above.
- If ready, reply "apply fixes" (or specify subset) to implement and run a typecheck.

Saved in: webui-fastify/todo-logging.md
