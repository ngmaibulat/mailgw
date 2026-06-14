Must-do before this can replace the original:

1. Migrations — the new service can't run at all without the DB schema. Two tasks: convert 12 Sequelize migration files to plain .sql and write a Bun migration runner. This is the most pressing remaining work.

2. Wire up /api/connection and /api/queue persistence — both currently just console.log. The models are already written (connection.ts, transaction.ts), so this is just connecting them in the route handlers.

Small loose ends:

3. checkMD5/hashLookup/hashListLookup — the model files (blockmd5.ts, hashlookup.ts) are already done; this task is just porting the orchestration logic from functions.js into a src/query/hash.ts or similar. Low priority if the attachment-blocking feature isn't actively used.
4. Remaining models (Header, Log, Config, RelayGroup, Relay, User, Exception) — all unused in current routes, safe to skip until needed.

Before going to production:

5. Authentication — the API has no auth. An API key middleware would be the minimal fix.
6. Testing — smoke tests for all endpoints.

Suggested order: Migrations → wire up connection/queue persistence → auth → testing.

---

Three things stand out, in order of importance:

1. Dockerfile for logservice-bun — the docker-compose.yaml already references ngmaibulat/mailgw-logservice-bun:latest but there's no Dockerfile yet. The service can't be deployed without it. This is the most urgent gap.

2. Error handling in route handlers — right now if the DB is down or a query throws, the error bubbles up unhandled and Bun will return a 500 with no useful response body. A simple try/catch in each route returning { status: "Error" } with HTTP 500 would make failures visible and safe.

3. Fix the Haraka bugs from the original code review — those are in production right now:

- hook_get_mx passes next(OK, false) when no route is found — should be DENYSOFT
- npFilter.js crashes on missing/malformed config with no null guard
- hook_get_mx will throw if rcpt_to is empty
