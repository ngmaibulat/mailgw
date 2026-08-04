# logservice-go

The audit-trail API for the mail gateway fleet, and the owner of the shared
MariaDB schema. A Go rewrite of the Bun/TypeScript service now frozen at
`legacy/logservice/`, wire-compatible
with it — see [Differences from the Bun service](#differences-from-the-bun-service),
which is a list of exactly one thing.

```bash
go build ./... && go vet ./... && go test -race ./...

# apply pending migrations and exit
DB_HOST=127.0.0.1 DB_USER=mailgw DB_PASS=... DB_NAME=mailgw \
  go run ./cmd/logservice migrate

# migrate, then serve
DB_HOST=127.0.0.1 DB_USER=mailgw DB_PASS=... DB_NAME=mailgw PORT=3000 \
  go run ./cmd/logservice

./bump.sh && ./container-build.sh    # the version bump is SEPARATE from the build
```

## It owns the schema for the whole stack

Not just the log tables. Fifteen of the tables `migrations/` creates — `Users`,
`Sessions`, `Relays`, `RelayGroups`, `Gateways`, `ConfigProfiles`,
`ConfigVersions`, `ConfigDeployments`, `GatewayAssignments`, `GatewayMetrics`,
`CredentialSets`, `SmtpCredentials`, `Logs`, `Exceptions`, `Configs` — are read
and written only by `webui-fastify`. The console describes them in
`db/schema.ts` but deliberately does not own or migrate them, which is why there
is no `drizzle-kit` there.

**A migration that does not run here is a console that does not work.**

## Migrations

Plain numbered `.sql` files in `migrations/`, embedded with `go:embed`, applied
in lexicographic order, tracked by **filename** in a `_migrations` table.

They are **byte-identical copies** of `legacy/logservice/migrations/*.sql`, and CI
asserts that (`.github/workflows/logservice-go.yml`). That matters because the
tracking key is the filename: a production database migrated by the frozen Bun
runner sees all 26 as already applied and this service changes nothing on its
first boot. **Never rename or edit a shipped file** — an existing database will
not re-apply it, so the edit changes nothing in production and everything on a
fresh install. Add `027_*.sql` and upward, here only.

Constraints on writing one, unchanged from the Bun service:

- Keep the zero-padded three-digit prefix; sort order is apply order.
- A file is executed **whole**, in one statement, so several statements per file
  is fine. That is why the migration connection — and only it — enables the
  driver's `multiStatements`; the request-serving pool never does.
- **MariaDB auto-commits DDL**, so a file that fails halfway leaves its earlier
  statements applied and its `_migrations` row unwritten. Prefer
  `CREATE TABLE IF NOT EXISTS`. Six shipped files use non-idempotent
  `ALTER TABLE ... ADD COLUMN` / `CREATE INDEX`, and the failure message says so.
- No down migrations.

Migrations run **automatically on `serve`**, before the listener binds, and
since M22 this is the **only** thing that migrates the shared schema — the
one-shot `db-migrator` service is gone from both compose files, and the console
waits at boot for the tables it reads instead of waiting for a sibling container
to exit 0.

The `migrate` subcommand still exists, for the failure case rather than the
normal one. `deploy/core/upgrade.sh` runs it **before** recreating services, so
a bad migration aborts the upgrade with the old stack still serving; left to
`serve`, the same migration is fatal and the container restart-loops. It is also
the foreground way to reproduce that failure once, with a clean exit code.

## This binary IS configured by its environment

`mailgw-go` is not, and its CI greps for `os.Getenv` to prove it. **That rule
does not apply here and must not be copied into this module.** A gateway is a
zero-configuration edge node that takes everything from Central Management,
because it runs on a host its operator does not otherwise touch. This runs on
the core node, beside the database whose credentials it needs.

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `3000` | listen port |
| `API_KEY` | *(unset)* | required in `X-API-Key` when set. **Unset means every request is accepted** |
| `DB_HOST` | `127.0.0.1` | MariaDB host |
| `DB_PORT` | `3306` | MariaDB port |
| `DB_USER` / `DB_PASS` / `DB_NAME` | *(none)* | credentials and schema |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `LOG_FORMAT` | `json` | `json` \| `text` |

## Routes

| Route | Notes |
|---|---|
| `GET /` | health check, `{"status":"OK"}`. **Open** — no API key |
| `POST /api/connection` | **no validation**; every absent field defaults |
| `POST /api/queue` | **no validation**; stored as a `Transaction` row |
| `POST /api/delivery` | validated; `200 {"status":"OK"}` or `400 {"status":"Fail"}` |
| `GET /api/{connection,delivery,transaction,hashlookup}` | search |
| `POST /filter/md5` | attachment blocklist; bare JSON array in |
| `GET /healthz` | liveness, no I/O. **New** |
| `GET /readyz` | migrated + database reachable. **New** |
| anything else | `404`, plain text `Resource does not exist\n` |

A wrong method on a known path answers that same 404, not a 405 — verified
against the Bun service, which falls through to its catch-all the same way.

### The status codes are load-bearing

`mailgw-go/internal/events` treats **any 4xx as terminal**: the audit event is
spilled to the gateway's disk and never retried. A transient failure here must
be a **5xx**. And `mailgw-go/internal/attach` turns any non-2xx, unparseable
body, missing `action` or unrecognised `action` from `/filter/md5` into an
error, which under `attach.fail: closed` becomes SMTP `451` and defers real
mail — so that endpoint answers `allow` for anything it can decide.

## Search

`GET` endpoints take a JSON `q` parameter:

```json
{ "search": [{"field": "sender", "operator": "contains", "value": "@ngm.dev"}],
  "searchLogic": "AND", "sort": [{"field": "dt", "direction": "desc"}],
  "limit": 100, "offset": 0 }
```

A malformed `q` yields the defaults — never a 400. Fields are checked against
per-table allowlists in `internal/query/fields.go`.

> **`BuildWhere` silently skips a field it does not recognise.** A column added
> to a table and to a grid but forgotten in the allowlist yields a filter that
> appears to work and **returns every row**. Add to the allowlist in the same
> commit as the column. Startup now checks this against the live schema: an
> allowlisted field that is not a column **refuses to start**, and a column
> nobody allowlisted logs a warning naming it.

`limit` is **clamped to 1000** (`query.MaxLimit`), which the Bun service did not
do — it put the value straight into `LIMIT` with no ceiling. Clamped rather than
rejected, because a 400 means something specific to the gateway (see above).

## Differences from the Bun service

Exactly one, and it is deliberate:

**`DATETIME` columns are returned as `"2026-08-03 18:34:37"`, not
`"2026-08-03T18:34:37.000Z"`.** The DSN does not set `parseTime`. The console's
grid formatter is `String(p.value).slice(0,19).replace("T"," ")`, so both render
identically, and nothing else reads a date out of this API. The alternative
would attach a timezone the stored value does not have — a MariaDB `DATETIME`
carries none — so the API would start reporting a different instant from the one
an operator sees running the same `SELECT` in `mysql`.

Everything else is byte-identical, including the JSON *types*: integers are
numbers, doubles are numbers, NULL is `null`. That is what `internal/rows`
exists for — a naive `[]byte` scan would have made every value a string.

## No healthcheck in compose, and why

The runtime image is `distroless/static`: no shell, no `curl`. A compose
`HEALTHCHECK` cannot be written the usual way. If one is wanted, add a
`logservice healthcheck` subcommand that self-dials `/readyz` and use that as
the test — do not add a shell to the image.

M22 is where that would have been built and was not. Deleting `db-migrator`
needed *something* to tell the console the schema existed, and this — plus
`webui: {condition: service_healthy}` — was the obvious candidate. The console
waits for its own tables instead, so the gate holds for a console started by
hand against a fresh database, with no compose file anywhere near it. Nothing
here needs a healthcheck to bring the stack up; one would still be worth having
for an orchestrator that restarts on it.

## Dependencies: one

`github.com/go-sql-driver/mysql`. Everything else is stdlib — no router (the
route set is nine entries and `net/http.ServeMux` has method patterns), no
validation library (the schema is four regexes), no migration library (the
runner is ~120 lines and its contract is fixed by an existing production
database).
