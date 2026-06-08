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
