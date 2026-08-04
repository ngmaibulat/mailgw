# logservice

A Go HTTP service (`net/http`) over MariaDB. Receives audit events from gateways
and serves searches to the console. It is also the **owner of the shared
database's schema**.

The module is `logservice-go/`. The original Bun/TypeScript implementation is
frozen under `legacy/logservice/` — see
[The Bun service is frozen](#the-bun-service-is-frozen).

```bash
cd logservice-go
go build ./... && go vet ./... && go test -race ./...

go run ./cmd/logservice migrate   # apply pending migrations and exit
go run ./cmd/logservice           # migrate, then serve
./bump.sh && ./container-build.sh # the version bump is SEPARATE from the build
```

## It owns the schema

**This is the only thing that migrates the database.** The console describes the
tables it queries in `db/schema.ts` but does not own them, and there is no
`drizzle-kit` there for that reason.

Fifteen of the tables belong to the console alone — `Users`, `Sessions`,
`Relays`, `RelayGroups`, `Gateways`, `ConfigProfiles`, `ConfigVersions`,
`ConfigDeployments`, `GatewayAssignments`, `GatewayMetrics`, `CredentialSets`,
`SmtpCredentials`, `Logs`, `Exceptions`, `Configs`. A migration that does not run
here is a console that does not work.

Adding a column means: write `logservice-go/migrations/NNN_*.sql`, add it to the
matching allowlist in `internal/query/fields.go`, describe it in the console's
`db/schema.ts`, then use it.

## Migrations

Plain numbered `.sql` files in `logservice-go/migrations/`, embedded with
`go:embed` and applied in order by `internal/migrate`, which tracks applied
**filenames** in a `_migrations` table.

Migrations run **automatically on start**, before the listener binds — the Bun
service did not do this, which is why a separate `db-migrator` container existed.
**M22 deleted that container**: this is now the only thing that migrates the
shared schema, and the console waits at boot for the tables it reads
(`webui-fastify` `db/index.ts` `waitForSchema`) instead of gating on a sibling
service exiting 0.

`logservice migrate` still exists and still migrates-and-exits. Nothing in
compose runs it; `deploy/core/upgrade.sh` does, **before** recreating services,
so a bad migration aborts the upgrade with the old stack still serving. Left to
`serve` alone, a fatal migration is a restart loop.

::: danger The filenames are the upgrade contract
`_migrations` records a migration by filename — no checksum, no ordinal. Files
001–026 are **byte-identical copies** of the ones the Bun runner applied, and CI
asserts that, so a production database sees all 26 as already applied.

**Never rename or edit a shipped file.** An existing database will not re-apply
it, so the edit changes nothing in production and everything on a fresh install.
Add 027 and upward.
:::

Behaviour that constrains how you write one:

- Files are sorted **lexicographically**, so keep the three-digit prefix.
- The whole file is executed as one statement, so several statements per file is
  fine. That is why the migration connection — and **only** it — enables the
  driver's `multiStatements`; the request-serving pool never does, because the
  search path builds SQL from caller-supplied JSON.
- **A failed file is not recorded** and aborts the run with a non-zero exit.
  MariaDB auto-commits DDL, so a file that fails halfway leaves its earlier
  statements applied — the error message says so. Write idempotent DDL
  (`CREATE TABLE IF NOT EXISTS`) where you can.
- No down migrations.

::: tip Migrations here are essay-commented, and that is the convention
Every migration from 017 onward opens with a comment block explaining why the
column exists and what it is *not*. Read `025_add_relay_use_mx.sql` or
`026_create_credential_sets.sql` before writing a new one.
:::

No foreign keys anywhere — referential integrity is enforced in the application.
Booleans are `TINYINT(1) NOT NULL DEFAULT 0`. `createdAt`/`updatedAt` are
`DATETIME NOT NULL` with **no** database default; the application fills them.

## Routes

| Route | Purpose |
|---|---|
| `GET /` | health check, **open** (no API key) |
| `POST /api/connection` | inbound connection events, **no validation** |
| `POST /api/queue` | queue events, stored as `Transaction` rows, **no validation** |
| `POST /api/delivery` | delivery events, **validated** |
| `GET /api/{connection,delivery,transaction,hashlookup}` | search |
| `POST /filter/md5` | attachment blocklist check |
| `GET /healthz` | liveness, no I/O |
| `GET /readyz` | migrated **and** database reachable |
| anything else | `404`, plain text `Resource does not exist\n` |

Auth checks `X-API-Key` against `API_KEY`; **when `API_KEY` is unset, every
request is accepted**. A wrong method on a known path answers the same 404 as an
unknown path, not a 405 — matching the Bun service, which fell through to its
catch-all the same way.

::: danger The status codes are load-bearing, in both directions
`mailgw-go/internal/events` treats **any 4xx as terminal**: the audit event is
spilled to the gateway's disk and never retried. A transient failure here must be
a **5xx**, and 4xx is reserved for "this body can never be stored".

`mailgw-go/internal/attach` turns any non-2xx, unparseable body, missing `action`
or unrecognised `action` from `/filter/md5` into an *error*, which under
`attach.fail: closed` becomes SMTP `451` and defers real mail. That endpoint
answers `allow` for anything it can decide and 500 only when the database could
not be consulted.
:::

## Search

`GET` endpoints take a JSON `q` parameter:

```json
{ "search": [{"field": "…", "operator": "…", "value": "…"}],
  "searchLogic": "AND", "sort": [{"field": "id", "direction": "desc"}],
  "limit": 50, "offset": 0 }
```

A malformed `q` — unparseable, a bare string, an array — yields the defaults,
never a 400. Fields are checked against per-table allowlists in
`internal/query/fields.go`.

::: danger BuildWhere silently skips a field it does not recognise
So a column added to a table and to a grid but forgotten in the allowlist yields
a filter that appears to work and **returns every row**. Add to the allowlist in
the same commit as the column.

Startup now checks this against the live schema: an allowlisted field that is
**not** a column **refuses to start** (the binary and the database disagree), and
a real column that no allowlist names logs a warning identifying it.
`createdAt`/`updatedAt` are excluded deliberately and do not warn.
:::

Each search returns the real `total` matching-row count — a `COUNT(*)` over the
same `WHERE` — not just the page size, so clients can paginate.

`limit` is **clamped to 1000**, which the Bun service did not do: it put the
value straight into `LIMIT` with no ceiling, so `{"limit":100000000}` was an
unauthenticated request to serialise a table that grows forever. Clamped rather
than rejected, because a 400 means something specific to the gateway.

## Data access

Raw SQL through `database/sql` with `github.com/go-sql-driver/mysql` — the
module's **only** dependency. There is no ORM, no router, no validation library
and no migration library.

`internal/rows` turns a `SELECT *` result set into JSON with the same types the
Bun service produced, reading `sql.Rows.ColumnTypes()` per column. Without it a
`[]byte` scan would make every value a JSON string — `"id": "42"` instead of
`"id": 42` — which the console's grids render identically, so the regression
would have been silent.

::: warning One deliberate wire difference: DATETIME formatting
A `DATETIME` is returned as `"2026-08-03 18:34:37"`, not
`"2026-08-03T18:34:37.000Z"`. The DSN does not set `parseTime`.

The console's formatter is `String(p.value).slice(0,19).replace("T"," ")`
(`public/js/grids/aggrid-common.js`), so both render identically, and nothing
else reads a date out of this API. `parseTime` would attach a timezone the stored
value does not have — a MariaDB `DATETIME` carries none — so the API would report
a different instant from the one an operator sees running the same `SELECT` in
`mysql`.

**This is the only intended difference between the two implementations.**
Anything else that differs is a bug.
:::

## It is configured by its environment, unlike the gateway

`mailgw-go` reads no environment variables and its CI greps for `os.Getenv` to
prove it. **That rule is specific to the gateway and must not be copied into this
module.** A gateway is a zero-configuration edge node that takes everything from
Central Management, because it runs on a host its operator does not otherwise
touch. logservice runs on the core node, beside the database whose credentials it
needs.

`PORT`, `API_KEY`, `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASS`, `DB_NAME`,
`LOG_LEVEL`, `LOG_FORMAT`.

## What the log rows carry

Every row has a `gateway` column naming which box wrote it, and a `Delivery` row
also has `route_rule` — the rule that sent that recipient there. Both are
optional on the wire and NULL for anything written before they existed.

`route_rule` is **per recipient**, not per envelope: one envelope groups by relay
group and can hold recipients routed there by different rules. That is why there
is no `route_rule` on `Transaction`.

## The Bun service is frozen

`legacy/logservice/` still builds and its unit suite still runs
(`pnpm test:logservice`),
kept for one release so a rollback is an image-tag edit —
`image: ngmaibulat/logservice:latest` — rather than a revert. Both compose files
now run `ngmaibulat/logservice-go:latest`.

Prefer changing `logservice-go/`. Touch `legacy/logservice/` only to keep it
building.
