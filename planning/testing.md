# Testing — plan

Consolidated testing tasks across the repo: the Haraka plugins (root package)
and `logservice-bun`.

## Done (context)

- **Haraka plugins** — `node:test` unit suite in `tests/` (`pnpm test`):
  `Route` / `RoutingTable`, `functions` (helpers, `getAddr`, `log_*`,
  `buildConnInfo`, `postWithLogging`, API-key headers), `npFilter`, `npRoute`.
- **logservice-bun** — `bun:test` unit suite in `logservice-bun/tests/`
  (`bun test`): delivery validation, query builder operators, auth middleware,
  hash decision logic (`decideActions`).

## Remaining

1. **logservice-bun live-DB integration tests** (highest value)
   Docker-based tests with a live DB: smoke-test the POST endpoints
   (`/api/connection`, `/api/queue`, `/api/delivery`, `/filter/md5`) against a
   real MariaDB (the `docker compose` db) and assert rows are actually written.
   Today's tests are unit-level only; nothing exercises a real DB write. Main
   confidence gap before production.

2. **End-to-end SMTP tests**
   Live tests that send mail via SMTP through Haraka (e.g. `swaks`) and assert
   the routing/filtering/logging behaviour end to end — connection allow/deny,
   route selection, and that the expected events reach the logservice.

3. **Haraka logging-plugin hook tests** (optional / lower value)
   `npConnection` / `npData` / `npQueue` / `npLogDelivery` hooks aren't covered
   directly. They mostly shape-and-POST, and the shaping is already exercised
   via the `functions` tests, so add these only if those plugins gain logic.
