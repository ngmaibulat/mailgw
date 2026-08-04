# M21 — logservice in Go

**Status:** **done** (2026-08-03)  ·  **Packages:** `logservice-go/` (new), `legacy/logservice/` (frozen, moved), `docker-compose.yaml`, `deploy/core`, `.github/workflows`, `docs/internal`  ·  **Depends on:** —  ·  **Blocks:** —

> A rewrite, not a feature. The whole milestone is judged by one question:
> **does `tests/api/logservice.e2e.test.ts` pass unmodified against the Go
> binary?** It does — 9/9 with `API_KEY` set, and 114/115 across every e2e suite
> on a clean stack. Everything below exists to make that answer yes without
> anybody having to touch a caller.
>
> The two places this was most likely to go wrong are named as such:
> [`SELECT *` → JSON typing](#a-select---json-with-the-right-types-the-main-compatibility-risk)
> and [multi-statement migration files](#b-executing-a-multi-statement-migration-file).
> Both landed as planned. Read
> [What was built differently](#what-was-built-differently) before using the rest
> of this file as a description of the code.

## Context

`logservice` is the Bun/TypeScript service that every gateway posts its audit
trail to and that the console reads its log grids from. It is also — and this is
the part that surprises people — **the only thing that migrates the shared
MariaDB**, including the fifteen tables only `webui-fastify` touches (`Users`,
`Sessions`, `Relays`, `Gateways`, `ConfigProfiles`, `ConfigVersions`,
`CredentialSets`, …). A rewrite that gets the schema half wrong breaks the
console, not just the log viewer.

Two things motivate moving it to Go:

1. **The stack is converging on Go.** The gateway is already a Go module with a
   settled house style — stdlib `net/http`, no framework, `log/slog`,
   distroless/static, `gofmt`+`vet`+`go test -race` as the whole gate. A second
   Go service means one toolchain, one deployment shape and one set of
   conventions across the two services that carry mail data.
2. **Migrations are a manual step today, and that has already cost.** The Bun
   entry point does *not* migrate on start (`logservice/src/index.ts:24-33`); it
   only migrates under `--migrate`, which is why both compose files carry a
   separate `db-migrator` service. The user's requirement here is explicit:
   **migrations run automatically on start, and each migration is a visible
   `.sql` file** — not a Go statement list like `mailgw-go/internal/store`.

The intended outcome is a `logservice-go/` module that is **wire-compatible to
the byte** with the Bun service, so `mailgw-go/internal/events`,
`mailgw-go/internal/attach`, `webui-fastify/src/logservice.ts` and
`tests/api/logservice.e2e.test.ts` all work unchanged, and a rollback is an
image-tag edit rather than a revert.

### Decisions already taken (do not relitigate)

- **New directory `logservice-go/`**, its own Go module, outside the pnpm
  workspace — the `mailgw-go/` precedent. The Bun service is **frozen but left
  buildable** for one release and now lives at **`legacy/logservice/`**, beside
  `legacy/mailgw` and `legacy/webui-express`; compose switches to
  `ngmaibulat/logservice-go`.
- **The 26 migration files are copied byte-identically** into
  `logservice-go/migrations/` under the same names and embedded with
  `go:embed`. `_migrations` keys on filename, so an existing production database
  sees all 26 as applied and nothing re-runs. Migrations 027+ are authored only
  in the Go tree.
- **Three gaps close with the rewrite**: a bound on `limit`, `/readyz` +
  server/query timeouts, and `log/slog` plus a startup check that the search
  field allowlists match the real columns.

### One rule that does *not* carry over

`mailgw-go`'s CI asserts the binary reads **no** environment variables
(`.github/workflows/go.yml:80-83`). That rule belongs to the zero-configuration
*gateway*, which runs on hosts its operator does not configure. logservice is a
core-node service configured by whoever deploys it; it reads `PORT`, `API_KEY`,
`DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASS`, `DB_NAME` and must keep doing so.
**Do not copy that CI step into `logservice-go`.** Say so in the module's
`README.md`, or somebody will "fix" it.

---

## The contract that must not move

Established by reading the Bun source and every caller. Anything not listed here
is free; everything listed is load-bearing.

### Routes

| Method + path | Behaviour |
|---|---|
| `GET /` | 200, body **exactly** `{"status":"OK"}`. **No auth**, no error wrapper — it is the healthcheck. |
| `POST /api/connection` | **No validation.** Every absent field defaults. 200 `{"status":"OK"}`. |
| `POST /api/queue` | **No validation.** Writes a `Transaction` row (not a Connection row — the double-insert is commented out at `logservice/src/routes/api.ts:33`). 200 `{"status":"OK"}`. |
| `POST /api/delivery` | Validated. 200 `{"status":"OK"}` \| **400 `{"status":"Fail"}`**. |
| `GET /api/{connection,delivery,transaction,hashlookup}?q=` | `{"status":"success","total":<int>,"records":[…]}`. |
| `POST /filter/md5` | Bare JSON **array** in; `{"action":"allow"\|"block"}` out. |
| anything else | 404, plain-text body `"Resource does not exist\n"`. |

- `X-API-Key` is compared against `API_KEY`, read once at startup. **Unset or
  empty ⇒ every request accepted**, with a startup warning. Mismatch ⇒ 401
  `{"status":"Unauthorized"}`. Panic/error ⇒ 500
  `{"status":"Error","message":"Internal server error"}`.
- **A POST must have committed before it replies** — `tests/api/logservice.e2e.test.ts:49-57`
  searches for the row immediately after.

### Why the status codes are not negotiable

`mailgw-go/internal/events/client.go:303-329` treats **any 4xx as terminal**: the
event is spilled to `queue/failed-events/` and never retried. Everything else is
transient and retried. So a Go logservice must never answer 4xx for anything it
would like to receive again — a transient DB failure is a **5xx**. Symmetrically,
`mailgw-go/internal/attach/scan.go:121-138` turns any non-2xx, unparseable JSON,
missing `action` or unknown `action` value from `/filter/md5` into an *error*,
which becomes SMTP `451` and defers real mail. `/filter/md5` answering something
creative is a mail outage.

### Wire details that are easy to get wrong

- **`dt` is epoch milliseconds** in every POST body, stored as
  `FROM_UNIXTIME(dt / 1000)`.
- **`port` on `/api/delivery` is a digit *string***, into an `INT` column.
- `Connection` mixes casing on purpose: `remoteAddr`/`remotePort` camel,
  everything else snake. Renaming one silently writes NULLs, because that
  handler defaults rather than rejects.
- `rcpt_list`/`rcpt_accepted` on delivery are **single addresses**; on queue,
  `rcpt_list` is a comma-joined list.
- Legacy Haraka still sends `state` and `pipelining` on connection events, which
  have no column — keep ignoring them. It also still sends comma-joined
  `rcpt_list` on delivery, which 400s today; **keep 400ing it**, or rows the
  current schema cannot represent start landing.
- `createdAt`/`updatedAt` are `NOW()`, filled by the application — the columns
  have no database default.

### Search semantics (`logservice/src/query/builder.ts`)

- `q` that is absent, unparseable, `null`, a scalar or an array ⇒ `{}` and all
  defaults. **Never a 400.**
- A search param is skipped when `!value && value !== 0` (drops `""`, null,
  false; **keeps `0`**) or when `field` is not in the table's allowlist —
  **silently, never a 400.** Preserve this; it is what the console relies on.
- Operators → SQL: `is`/`=` → `= ?`; `begins`/`contains`/`ends` → `LIKE` with the
  wildcard on the right side, left side, both; `between` → `BETWEEN ? AND ?`
  **only** when the value is a 2-element array, else the whole condition is
  dropped; `>`/`more`, `>=`, `<`/`less`, `<=`. Unknown operator ⇒ no clause.
  LIKE wildcards inside a user value are **not** escaped today — preserve that,
  the grids depend on `contains` behaving as a substring search and nothing more.
- `searchLogic` is re-normalised **at SQL-assembly time**, not only at parse
  time: anything that is not exactly `"OR"` uppercased becomes `"AND"`. Both
  layers exist deliberately (`builder.ts:95-101` has a "don't simplify this back"
  comment and `logservice/tests/builder.test.ts:113-159` pins it with injection
  payloads). **Port both layers and port the tests.**
- SQL issued: `SELECT COUNT(*) AS total FROM \`T\` [WHERE …]`, then
  `SELECT * FROM \`T\` [WHERE …] ORDER BY <orderBy> LIMIT ? OFFSET ?`. Defaults
  limit 100 / offset 0; order fallback `` `id` DESC ``. `total` is the real count
  ignoring limit/offset — AG Grid uses it as the exact last-row index
  (`webui-fastify/public/js/grids/aggrid-common.js:159-170`).
- `hashlookup` is the only JOIN: alias `h`, `LEFT JOIN \`Transaction\` t ON
  t.uuid = h.txn_uuid`, order fallback `` `h`.`id` DESC ``, selecting `h.*` plus
  the Transaction display columns with `t.action AS txn_action`. Only
  HashLookups columns are filterable and sortable.

Field allowlists (verbatim from `logservice/src/query/search.ts:16-40`):

- **Delivery** — `id uuid dt sender rcpt_domain rcpt_list rcpt_accepted
  tls_forced tls auth host ip port response delay gateway route_rule`
- **Connection** — `id uuid dt encoding hello_name remoteAddr remotePort
  remote_host remote_info remote_is_local remote_is_private using_tls tran_count
  rcpt_count_accept rcpt_count_tempfail rcpt_count_reject gateway`
- **Transaction** — `id uuid dt action encoding sender rcpt_list
  rcpt_count_accept rcpt_count_tempfail rcpt_count_reject delay_data_post
  data_bytes mime_part_count gateway`
- **HashLookups** — `id txn_uuid md5 contentType filename size action createdAt`

### `/api/delivery` validation (`logservice/src/validation/delivery.ts`)

Required: `uuid` string; `dt` number; `sender` **email or empty string** (the
null sender every bounce uses); `rcpt_domain` host; `rcpt_list` single email;
`rcpt_accepted` single email; `tls_forced`/`tls`/`auth` **strict** booleans
(`"yes"` is a 400); `host`; `ip` (IPv4, IPv6 incl. `::ffff:` form, **or empty**);
`port` digit-string; `response` string; `delay` number. Optional: `gateway`
(≤64), `route_rule` (≤255). Unknown keys **stripped, not rejected**.

`hostSchema` deliberately accepts single-label names (`localhost`,
`dev-mailhog`) and bare or bracketed IP literals — an FQDN-only rule silently
discarded rows for successful deliveries. It rejects empty, spaces, underscores
and a leading dash. Port that regex character-for-character.

### `/filter/md5` (`logservice/src/query/hash.ts`)

Bare array of `{md5?, contentType?, filename?, size?, txn_uuid?}`. A non-array
body is treated as `[]` (⇒ allow) — **do not 400 it**, `mailgw-go` turns a 4xx
into a deferred message. Overall `block` iff any md5 is in `BlockMD5s`. **Every**
attachment is inserted into `HashLookups`, including allows and md5-less ones
(with `""` defaults). Empty list ⇒ allow, and `mailgw-go` short-circuits without
calling at all.

---

## Design

### Module layout

```
logservice-go/
  go.mod                     module github.com/ngmaibulat/mailgw/logservice-go
  VERSION  bump.sh  container-build.sh  container-push.sh
  Dockerfile  .dockerignore  README.md  TODO.md
  cmd/logservice/main.go     flags + dispatch only
  migrations/                001…026_*.sql, byte-identical copies
  internal/
    db/       dsn.go, db.go, wait.go          — pool, DSN, readiness ping
    migrate/  migrate.go, embed.go, split.go  — go:embed FS + the runner
    rows/     rows.go                         — SELECT * → JSON, type-faithful
    query/    builder.go, search.go, fields.go — the q-param → SQL translation
    store/    connection.go, transaction.go, delivery.go, hashlookup.go, blockmd5.go
    validate/ delivery.go, host.go            — hand-written, mirrors the zod
    api/      server.go, routes.go, middleware.go, handlers.go, health.go
```

`cmd/logservice` parses flags and dispatches; everything real lives in
`internal/`, and `internal/api.Server` is the composition root — the
`internal/node` lesson from M19, one layer smaller. Each package opens with a doc
comment saying *why it exists*, per house style.

### Dependencies: exactly one

`github.com/go-sql-driver/mysql` — the only pure-Go MariaDB driver, and the
first MySQL client anywhere in the repo's Go code. Everything else is stdlib:

- **No validation library.** zod's semantics here are four regexes and a dozen
  type checks; a dependency to express that would be larger than the code.
- **No router.** `net/http.ServeMux` has method+wildcard patterns and the route
  set is nine entries.
- **No migration library.** The whole runner is ~120 lines and its contract
  (`_migrations` keyed by filename) is already fixed by the existing database.

Record this in `docs/internal/architecture/decisions.md` next to the gateway's
nine, as its own short table — the two modules have separate budgets.

### A. `SELECT *` → JSON with the right types  *(the main compatibility risk)*

Bun returns `SELECT *` rows straight to JSON with driver-assigned types: `INT` →
JSON number, `DOUBLE` → number, `VARCHAR`/`TEXT` → string, `NULL` → null,
`DATETIME` → a JS `Date` that stringifies to `"2026-08-03T12:00:00.000Z"`.

A naive `[]byte` scan in Go makes **everything a string** (`"id":"42"`), which is
a real wire change. `internal/rows` fixes that with `rows.ColumnTypes()` and
`DatabaseTypeName()`. The four queried result sets span only five types:

| MySQL type | Scanned into | JSON |
|---|---|---|
| `INT`, `BIGINT`, `TINYINT` | `sql.NullInt64` | number / null |
| `DOUBLE`, `FLOAT`, `DECIMAL` | `sql.NullFloat64` | number / null |
| `VARCHAR`, `TEXT`, `LONGTEXT`, `CHAR` | `sql.NullString` | string / null |
| `DATETIME`, `TIMESTAMP`, `DATE` | `sql.NullString` — see below | string / null |
| anything else | `sql.NullString` | string / null — the safe default, and a new column type is then a display quirk rather than a scan panic |

**`DATETIME` stays the raw MySQL string** (`"2026-08-03 12:00:00"`), i.e. the DSN
does **not** set `parseTime=true`. Two reasons:

1. The grid's formatter is
   `String(p.value).slice(0,19).replace("T"," ")`
   (`webui-fastify/public/js/grids/aggrid-common.js:27-29`), so
   `"2026-08-03T12:00:00.000Z"` and `"2026-08-03 12:00:00"` render **identically**.
   Nothing else reads `dt` from the API.
2. `parseTime=true` would introduce a timezone the Bun version never had: a
   MariaDB `DATETIME` carries none, so Go would stamp it with the connection's
   location and the grid would show a value that differs from what the same
   operator sees in `mysql` by the UTC offset. Choosing the raw string means the
   API and the database agree, which is the property worth having in an audit
   log.

This is a **deliberate, documented difference** from the Bun service: the string
loses its `T`, its `.000` and its `Z`. Write it in `logservice-go/README.md` and
in `docs/internal/packages/logservice.md`, because the next person to see
`2026-08-03 12:00:00` in a `curl` will otherwise think it is a bug.

Rows are built as `map[string]any`. Go marshals map keys **alphabetically**,
losing MySQL's column order — that is fine: AG Grid, the w2ui viewers and the
dashboard all index by field name, and `tests/api/logservice.e2e.test.ts` reads
`records[0].<field>`. Nothing in the repo depends on JSON key order. Say so in a
comment so it is a decision rather than an accident.

`internal/rows` is testable without a database using `go-sql-driver`'s
`sql/driver` interfaces — but the cheaper and more honest test is a **golden
file**: one fixture per result set, `UPDATE_GOLDEN=1` to regenerate, exactly the
`internal/msgauth/results_test.go:33-52` pattern. The end-to-end proof is
`tests/api/logservice.e2e.test.ts` run against both binaries.

### B. Executing a multi-statement migration file

The Bun runner passes each whole file to one `db.unsafe(sql)`. Six files (015,
020, 022, 023, 025, 026) hold several statements plus `--` essay comments.
`go-sql-driver/mysql` rejects multiple statements unless the DSN carries
`multiStatements=true`.

**Take a dedicated migration connection with `multiStatements=true`, opened and
closed around the run, entirely separate from the request-serving pool** — which
must never have it, because multi-statement support turns any injection from a
one-statement problem into an arbitrary-script one, and the search path is
exactly where untrusted input reaches SQL.

Rejected: a hand-written splitter. It buys a better error message ("statement 3
failed") at the cost of a parser that has to understand `--` and `/* */`
comments, single and double quotes, backtick identifiers and `$$`-style bodies,
and that is wrong the first time somebody writes a migration containing a
semicolon in a string literal. The `internal/store` runner prefers statement
lists for exactly the error-attribution reason — but that is a Go array where the
statements are already separate, not a parser. Here the file *is* the artefact.
MariaDB's syntax errors name the offending SQL text, which is enough.

Neither option gives atomicity: **MariaDB DDL auto-commits**, so a file that
fails halfway leaves the earlier statements applied and the `_migrations` row
unwritten. That is the behaviour today and the plan does not change it — but it
does two things about it:

1. **Log loudly and specifically on failure**: the filename, the driver error,
   and the sentence *"the schema may be partially migrated; this file's
   statements are not all idempotent — inspect the schema before re-running"*.
   Today the message is `migration <file> failed: <reason>` and stops there.
2. **Refuse to serve.** A failed migration aborts startup with a non-zero exit,
   never a degraded server. Same as today's `--migrate`, now on every boot.

Down migrations remain out of scope, as they are today.

### Migrations on start — and what happens to `db-migrator`

`internal/migrate.Run(ctx, db)` executes: create `_migrations` if absent → read
applied filenames → sort the embedded `.sql` names lexicographically → apply each
unapplied file on the multi-statement connection → insert its name. Identical
semantics to `logservice/src/dbmigrate.ts`, including the ordering and the
absence of checksums.

It runs from **two** places:

- `logservice serve` (the default, no subcommand) — migrate, **then** bind the
  listener. This is the user's requirement.
- `logservice migrate` — migrate and exit 0/1. **This subcommand still has to
  exist**, because `webui-fastify` in both compose files gates on
  `db-migrator: service_completed_successfully` for schema readiness, and the
  webui does not talk to logservice at boot. Dropping the service would let the
  console start against an unmigrated database.

Compose diffs (both `docker-compose.yaml` and `deploy/core/docker-compose.yaml`):

```diff
   db-migrator:
-      image: ngmaibulat/logservice:latest
-      command: ["bun", "src/dbmigrate.ts"]
+      image: ngmaibulat/logservice-go:latest
+      command: ["migrate"]
   logservice:
-      image: ngmaibulat/logservice:latest
+      image: ngmaibulat/logservice-go:latest
```

`logservice` keeps `depends_on: db-migrator: service_completed_successfully` even
though it now migrates itself — the two are then racing on an already-migrated
database, which is a no-op, and removing the edge would let logservice and the
migrator run the same file concurrently on a **fresh** volume. Note in the
compose comment that the dependency is now about *ordering*, not readiness.

Concurrency note worth a comment in `migrate.go`: `_migrations.name` is
`UNIQUE`, so two racing runners cannot both record the same file — the loser gets
a duplicate-key error on the insert. That is a real ordering guard, not a
coincidence, and it is why the `UNIQUE` must survive into the Go tree unchanged.

### Startup, waiting, and readiness

- **Wait for the database** with the Bun runner's schedule — 10 attempts of
  `SELECT 1`, 2 s apart — before migrating (`logservice/src/dbmigrate.ts:13-38`).
  The server waits too, and for the same reason: it now migrates, and a
  logservice that binds before the schema exists answers 500 to every gateway,
  each of which spills the event to disk.
- **Fail fast after that.** Exhausting the retries is a non-zero exit with the
  host:port in the message, exactly as today. Docker restarts it; a process that
  half-works is worse.
- `GET /healthz` — liveness, no I/O, no credential.
  `GET /readyz` — 200 once migrations have completed **and** the pool answers a
  `SELECT 1` within a short deadline; 503 with a fixed reason string otherwise.
  Both are **new**; `GET /` stays exactly as it is, because the e2e suite asserts
  its body byte-for-byte and compose healthchecks may already point at it.
  Follow `mailgw-go/internal/adminui/observe.go:17-77`: fixed reason strings, no
  echoing of driver errors to an unauthenticated caller.
- Neither compose file gives logservice a healthcheck today, and
  `distroless/static` has **no shell and no curl**, so one cannot be added the
  usual way. If a healthcheck is wanted later, it is a `logservice healthcheck`
  subcommand that self-dials `/readyz` — worth a line in the module README so
  the next person does not discover it while debugging a container that "won't
  become healthy".

### The gaps being closed

**Bound `limit`.** `q.limit` goes straight into `LIMIT ?` today with no ceiling,
so `{"limit":100000000}` is an unauthenticated amplifier against a table that
grows forever. **Clamp, don't reject**: `limit` outside `[0, maxLimit]` becomes
`maxLimit` (1000), a negative or non-integer becomes the default 100. Rejecting
would be a 400 the console has never had to handle, and — via the events client's
4xx rule — a 400 is the one answer that must stay reserved for "this body will
never be acceptable". Log at Debug when a clamp happens. `offset` gets the same
non-negative-integer coercion.

**Timeouts.** `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 30 s,
`IdleTimeout` 60 s on the `http.Server`, matching
`mailgw-go/internal/adminui/server.go:213-251`. Every query runs on the request
context with an added deadline (`queryTimeout`, 15 s), so a slow `COUNT(*)` over
a large table cannot pin a connection past the write timeout. `http.MaxBytesReader`
on every POST body — 1 MiB, the `internal/testctl` constant.

**slog.** JSON to stderr by default, `text` opt-in via `LOG_FORMAT`, level via
`LOG_LEVEL` — the `newLogger` shape from `mailgw-go/internal/node/node.go:214-230`.
Key/value attrs only. **Never log the API key**, and never log a `q` value at
Info (it holds sender addresses).

**Allowlist-vs-columns check.** At startup, for each of the four tables, run
`SELECT * FROM \`T\` LIMIT 0` and read `rows.Columns()`. Compare against the
allowlist. This uses the same code path the search queries use, needs no
`information_schema` grant, and costs one round trip per table.

- An allowlisted field that is **not** a real column ⇒ **refuse to start**. It
  can only produce a broken query, and it means the deployed binary and the
  deployed schema disagree.
- A real column **missing** from the allowlist ⇒ **warn, with the column name**.
  This is the failure mode `logservice/src/query/search.ts:11-14` and migration
  023's comment both warn about — a filter that appears to work and returns every
  row. It must not be fatal, because `createdAt`/`updatedAt` are deliberately
  unlisted on three of the four tables; the check therefore carries an explicit
  `knownUnfiltered` set, and the warning is only for a column in neither list.

---

## Files

New, all under `logservice-go/` (see the layout above). The ones worth naming:

- `internal/query/builder.go` — `BuildWhere(params, logic, allowed, prefix)` and
  `BuildOrderBy(sort, allowed, fallback, prefix)`, a straight port of
  `logservice/src/query/builder.ts` including **both** `searchLogic`
  normalisation layers.
- `internal/query/fields.go` — the four allowlists as `map[string]struct{}`, with
  the same "silently skipped, so add to this in the same commit as the column"
  comment the TypeScript carries.
- `internal/rows/rows.go` — `Scan(*sql.Rows) ([]map[string]any, error)`, the
  type-faithful `SELECT *` reader.
- `internal/migrate/migrate.go` + `embed.go` — `//go:embed migrations/*.sql`
  lives here; `Run(ctx, *sql.DB) error`.
- `internal/validate/delivery.go` — the hand-written mirror of `schemaDelivery`,
  regexes included verbatim.
- `internal/api/routes.go` — the `ServeMux` table with middleware wrapped **per
  route**, so the table reads as the policy (`mailgw-go/internal/adminui/server.go:181-205`).

Modified outside the new module:

- `docker-compose.yaml`, `deploy/core/docker-compose.yaml` — image + command, as
  above.
- `package.json` (root) — add `build:logservice-go`, point `docker:push` at it,
  add `test:logservice-go`, and extend `check` (today `cd mailgw-go && gofmt -l
  . && go vet ./... && go test ./...`) to cover the second module; leave
  `build:logservice` for the frozen Bun image.
- `.github/workflows/go.yml` — either add `logservice-go/**` to the path filter
  with a second job, or add `logservice-go.yml` alongside it. Prefer a **second
  workflow file**: the gateway job carries two architectural assertions that must
  not run here.
- `docs/internal/packages/logservice.md` — rewrite for the Go service; keep the
  "it owns the schema" and "buildWhere silently skips" sections, which are still
  true and still the most important things on the page.
- `docs/internal/architecture/decisions.md` — the one-dependency table, and the
  DATETIME-format decision.
- `legacy/logservice/README.md` (new) — mark it frozen, point at
  `logservice-go/`. The package itself moves there with `git mv`.
- `plans/README.md` — the index row (added with this file).

---

## Phases

Each phase ends green on `gofmt -l`, `go vet ./...`, `go test -race ./...`.

**1 — Module skeleton and migrations.** `go.mod`, the copied `migrations/`,
`internal/db`, `internal/migrate`, `cmd/logservice migrate`.
*Verify:* `docker compose down -v && docker compose up -d mariadb`, then
`go run ./cmd/logservice migrate` twice — the first applies 26, the second says
all applied. Then point it at a database already migrated by the Bun runner and
confirm it applies **zero**.

**2 — Query layer, offline.** `internal/query`, `internal/rows`,
`internal/validate`. No HTTP yet.
*Verify:* ported unit tests. `logservice/tests/builder.test.ts` (252 lines,
including the `searchLogic` injection payloads) and `validation.test.ts` (149
lines) translate almost case-for-case into Go table tests — port them rather than
inventing new ones, so a divergence is a test failure and not a discovery in
production.

**3 — Store and handlers.** `internal/store`, `internal/api`.
*Verify:* `httptest` tests driven through `s.Handler()` — routing, the auth
middleware in both modes, the 404 body, the 400 body, the JSON envelopes.

**4 — Serve, health, wiring.** `serve` as the default subcommand, migrate-then-bind,
`/healthz`, `/readyz`, timeouts, the allowlist check, slog.
*Verify:* `go run ./cmd/logservice` against the dev MariaDB, then
`MAILGW_API_E2E=1 LOGSERVICE_URL=http://127.0.0.1:3000 bun test ./tests/api/` —
**unmodified**. That suite is the acceptance test for this whole milestone.

**5 — Image and compose.** Dockerfile, `VERSION`, the three shell scripts,
compose edits, CI workflow.
*Verify:* `docker compose down -v && pnpm start` on a clean volume; then
`pnpm test:e2e` (SMTP + API + stack). `tests/stack/delivery.test.ts` is the one
that proves a real gateway's events survive the new validator into MariaDB.

**6 — Docs and freeze.** The doc edits above, the status line on line 3 moved to
**done**, and a "What was built differently" section added to *this file*
recording every place this design turned out to be wrong — the house convention,
and the thing M14/M16/M20 each found most valuable in hindsight.

---

## Verification

The single most valuable check is that **`tests/api/logservice.e2e.test.ts` runs
unmodified and green against the Go binary**, because it is a black-box
assertion of the exact bodies (`{"status":"OK"}`, `{"status":"Fail"}`,
`{"action":"allow"}`), the 401, the 404 and the read-after-write guarantee.

Beyond it:

```bash
# unit
cd logservice-go && gofmt -l . && go vet ./... && go test -race ./...

# fresh-volume bring-up, migrations on start
docker compose down -v && pnpm start && docker compose logs logservice

# upgrade path: Bun-migrated DB, Go binary, zero migrations applied
docker compose up -d mariadb db-migrator   # old image
docker compose up -d logservice            # new image; expect "all migrations already applied"

# the full e2e
pnpm test:e2e
```

Two comparisons worth doing once, by hand, because no test asserts them:

- `curl -s 'localhost:3000/api/delivery?q={"limit":1}'` against **both** binaries
  and diff the JSON. Expect exactly one class of difference: `dt`, `createdAt`
  and `updatedAt` as `"2026-08-03 12:00:00"` rather than
  `"2026-08-03T12:00:00.000Z"`. **Any other difference is a bug.**
- Load `/logs/delivery`, `/logs/connection`, `/logs/mails` and `/logs/lookups` in
  the console and sort and filter each grid, including a `dt` filter and a
  `between`. The grids are the only consumer of the `total` field and the only
  thing that renders `dt`.

## What was built differently

Six departures from the plan above. None changed the shape of the milestone; two
were plan errors and one is a finding worth keeping.

**1. `go:embed` cannot reach out of its own package, so the SQL files carry their
own `embed.go`.** The plan put `//go:embed migrations/*.sql` in
`internal/migrate`. That does not compile — an embed pattern cannot escape the
directory of the package declaring it. The alternative was to move the files to
`internal/migrate/migrations/`, which works and buries them three directories
deep. They stayed at `logservice-go/migrations/` with a one-variable
`package migrations` beside them, because a migration is the artefact an operator
reads before an upgrade and the thing a reviewer diffs — and because somebody
coming from `logservice/migrations/` will look there.

**2. A wrong method answers 404, not 405 — and that matches Bun.** The plan
assumed Go's `ServeMux` would return its automatic 405 and a test was written to
assert it. It returns 404, because registering the `/` catch-all suppresses the
405 path. Rather than work around that, the Bun service was **measured**: a
request whose method is not in a route's method object falls through to its
catch-all `fetch` and answers `404 "Resource does not exist\n"`. The two
implementations already agreed; the test was wrong, and now says so with the
reason.

**3. The plan's "e2e asserts `GET /` byte-for-byte" was slightly wrong, and it
mattered less than it looked.** The suite uses `await res.json()` and
`toEqual`, so whitespace is irrelevant — which is what lets `writeJSON` end every
body with a newline without breaking anything.

**4. Two safety properties were added that the plan did not ask for.** The
API-key comparison is `crypto/subtle`, not `!==`: the key travels on every
request from every gateway, so it is exactly the kind of secret a timing oracle
gets many samples of. And `migrate.Check()` refuses to start on an empty embedded
set — a `go:embed` that stops matching is a successful build whose migrator
reports "schema up to date" against an empty database, which is the worst
failure this service could have.

**5. `/readyz` panicked on a nil pool, found by its own test.** `s.DB.PingContext`
on a nil `*sql.DB` panics; the recover middleware turned that into a 500. It is
unreachable in a wired-up `serve`, but readiness is the endpoint an operator hits
*precisely when something is wrong*, so it now answers 503 with
`database unreachable` in every state it can be reached in.

**6. CI grew an assertion the plan did not specify: the copied migrations must
still match the frozen originals.** `_migrations` keys on filename, so editing a
shipped file changes nothing in production and everything on a fresh install —
the two would silently diverge. A `cmp` loop over `logservice/migrations/*.sql`
catches that, and tolerates 027+ existing only in the Go tree.

### What the verification actually proved

Worth recording, because these were predictions in the plan and are now
measurements:

- **The schema is identical.** All 26 migrations applied to a fresh database
  produce byte-identical output to the Bun-migrated production schema —
  204 columns and 47 indexes, `diff`-clean.
- **The upgrade path is a no-op.** Pointed at the already-migrated dev database,
  the Go runner applies **zero** migrations and logs "schema up to date".
- **The JSON differs in exactly one way.** All four search endpoints were curled
  from both services and compared after normalising datetime punctuation: byte-
  identical, types included. Integers stayed integers, doubles stayed doubles,
  NULL stayed `null`. `internal/rows` did its job.
- **Both halves of the allowlist check fire.** Dropping `Delivery.route_rule`
  from a throwaway database refuses startup with exit 1 naming the field; adding
  an unlisted column warns and starts.

## Deliberately not done

- **No `/api/hashlookup` POST and no ingest changes.** `HashLookups` rows are
  written only as a side effect of `/filter/md5`, as today.
- **No down migrations, no checksums.** Both would change the meaning of the
  existing `_migrations` table, which has 26 rows in production.
- **The dead code stays dead.** `isMD5Blocked`, `upsertMailAddr` and
  `linkMailAddrToTransaction` have no callers; do not port them. `MailAddrs`,
  `linkAddrTransactions` and `Headers` keep their migrations and get no Go code.
- **The Bun service is not deleted.** It moved to `legacy/logservice/` and stays
  buildable for one release so a rollback is
  `image: ngmaibulat/logservice:latest` plus
  `command: ["bun", "src/dbmigrate.ts"]` on the migrator. Its unit suite is still
  wired into `pnpm test`.
