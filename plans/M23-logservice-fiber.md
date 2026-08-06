# M23 — `logservice-fiber`: a second logservice on Fiber v3

**Status:** **done** (2026-08-06)  ·  **Packages:** `logservice-go` (packages promoted, `migrate`, new `apitest`), `logservice-fiber` (new), `docker-compose.yaml`, `tests`, `.github/workflows`, `docs`  ·  **Depends on:** M21, M22  ·  **Blocks:** —

> An experiment with a control group. `logservice-go` keeps serving, unchanged
> in behaviour; a second module serves the same wire contract from the same
> database over Fiber v3, and the two are judged against each other by a suite
> both run and a differential test that compares bytes at the socket. The
> milestone is judged by one question: **does the differential test pass with
> nothing in its header-normalisation allowlist beyond `date`, `connection` and
> `keep-alive`?**

## Why

M21 built logservice in Go on `net/http` and recorded why there is no router:
the route set is nine entries and `ServeMux` has had method patterns since Go
1.22. That is still true. What it does not answer is what a framework would be
like here, and the honest way to find out is to build one and measure it rather
than to argue about it — particularly for a service whose status codes are
load-bearing in three other systems.

The constraint that shapes everything below is that **this service cannot be
experimented on in place**. `mailgw-go/internal/events` treats any 4xx as
terminal and spills the audit event to the gateway's disk rather than retrying;
`mailgw-go/internal/attach` turns any non-2xx from `/filter/md5` into SMTP 451
and defers real mail; `webui-fastify` maps a non-2xx to 502. A framework's
default 405 or 413 where this answers 404 or 400 is not a cosmetic difference —
it silently destroys audit rows or defers mail. So the second implementation
runs beside the first, and "it behaves identically" has to be a fact something
checks rather than a claim.

## What was decided, and against what

**A separate module, not a second `cmd` in `logservice-go`.** The M19 precedent
(`cmd/mailgw-go-test` beside `cmd/mailgw-go`, containment asserted with
`go list -deps`) would have worked and was the first proposal. It was rejected
for a blunter property: `logservice-go/go.mod` stays at one direct dependency.
Fiber pulls in fasthttp, brotli, klauspost/compress, bytebufferpool, tcplisten
and more — roughly ten modules — and `docs/internal/architecture/decisions.md`
gives this module its own dependency budget. A separate module keeps that budget
intact and makes the experiment's cost visible in exactly one `go.mod`.

**The non-HTTP packages are shared source, not copied source.** Go's `internal/`
rule blocks a sibling module from importing `logservice-go/internal/*`, so six
packages — `db`, `query`, `rows`, `store`, `validate`, `migrate` — move to the
top level. `internal/api` stays internal, and that boundary is the thesis of the
whole milestone: **the two implementations differ in their HTTP layer and in
nothing else**, which is what makes comparing them mean anything.

Copying them instead was the obvious alternative and is worse in a specific,
invisible way. `query/fields.go` documents its own failure mode: `BuildWhere`
**silently skips** a field it does not recognise, so an allowlist entry
forgotten in one copy yields a filter that appears to work and returns every
row. And `rows/rows.go` exists for one purpose — keeping the JSON *types*
identical, so an `id` comes back `42` and not `"42"`. Duplicating the two files
whose job is preventing invisible divergence, in a milestone whose goal is
byte-identity, defeats itself. Copying also would not have avoided the
cross-module link, because the contract suite needs it either way.

**The contract suite lives in `logservice-go/apitest` and imports neither
implementation.** It is the specification; they are the subjects. It also fixes
something `internal/api/server_test.go` was doing out of convenience: several
tests use "a nil `*sql.DB` panics" as a proxy for "auth ran first", which is an
incidental stdlib behaviour and does not port. The portable form of that
assertion is the one that actually matters — a broken database must answer
**500**, because a 4xx makes mailgw-go discard the event permanently — and it
holds whether an implementation gets there by panic recovery or by an error
path. *How* a 500 is produced is implementation detail and stays in each
module's own `server_test.go`; *that* it is a 500 is contract.

**Both binaries migrate, and that required fixing `migrate.Run` first.** The
plan's first draft had `logservice-fiber` wait for the schema rather than apply
it, on the grounds that one owner is better than two. That was overruled, and
the overruling exposed a real defect rather than merely a preference: `Run`
reads `appliedSet` once up front and records a file only *after* executing it,
so two runners starting together on a fresh volume both see an empty set and
both execute all 26 files. The `createTable` comment claims the `UNIQUE(name)`
stops the second *application* of the DDL. It does not, in this ordering — the
loser dies inside `ExecContext` on `duplicate column name`, never reaching the
`INSERT`, and under `restart: unless-stopped` that is a restart loop. Six files
are non-idempotent `ALTER TABLE ADD COLUMN` / `CREATE INDEX`.

M22's own "Why" section describes this race and deleted `db-migrator` to leave
one migrator; this milestone reintroduces a second one, so it pays the bill M22
avoided rather than inheriting it. A named MariaDB advisory lock held for the
whole run, taken on a dedicated session-scoped connection, with `appliedSet`
re-read after acquisition. It retroactively protects `deploy/core/upgrade.sh`
too, where a one-off `logservice migrate` can overlap a still-running container.

**Nothing in `deploy/core/` changes.** The new service exists in the dev compose
stack on port 3001 and nowhere else. Promoting it to production is a separate
decision to be made from differential evidence, not from this plan.

## What is being built

| | |
|---|---|
| `logservice-go/{db,query,rows,store,validate,migrate}/` | promoted out of `internal/`, pure moves |
| `logservice-go/migrate/migrate.go` | advisory lock around `Run` |
| `logservice-go/apitest/` | the contract suite + `driver.Connector` fakes |
| `logservice-fiber/` | new module: `cmd/logservice-fiber`, `internal/api`, Dockerfile, CI |
| `tests/api/logservice.differential.test.ts` | byte comparison at the socket |

## The Fiber differences that have to be defeated

Each is byte-asserted today, and Fiber's default violates it:

| behaviour | `net/http` | Fiber's default | how |
|---|---|---|---|
| wrong method on a known path | plain-text 404 | 405 + `Allow` | catch-all `app.Use` registered **last**, plus `ErrorHandler` mapping 404/405/501 |
| oversized body | 400 (500 on `/filter/md5`) | 413 | hand-rolled `readBody`, `BodyLimit` only a memory backstop |
| JSON content type | exactly `application/json` | varies by helper | never call `c.JSON`; port `writeJSON` line for line |
| JSON body | trailing `\n` | none | same |
| trailing slash, case | 404 | matches | `StrictRouting: true`, `CaseSensitive: true` |
| unknown method | 404 | 501 | the same `ErrorHandler` arm |
| DB deadline parent | `r.Context()` | `c.Context()` is `Background()` | parent off `c.RequestCtx()` |
| `ReadHeaderTimeout` | 10s | **no equivalent** | `ReadTimeout: 10s`, recorded as a known difference |

`SkipUnmatchedRoutes` must stay off (its default): it answers 404/405 *before*
the middleware chain and would restore the 405.

Two pieces of shipped middleware are deliberately not used. `middleware/keyauth`
is fail-closed, and an empty `API_KEY` means "accept everything" here — that is
the Bun service's behaviour, what the dev stack depends on, and warned about at
startup rather than per request. `middleware/recover`'s output goes through the
default error handler and is not the `{"status":"Error",…}` envelope. Both are
hand-written, ported from `internal/api/middleware.go`.

## Order of work

1. Promote the six packages. Gate: `gofmt -l . && go vet ./... && go test -race ./... && go build ./...`
   green with **zero changes to test logic**. Commit alone — it should review as a move.
2. The advisory lock, with a test that runs two `Run` calls concurrently against
   one database and asserts both return nil and all 26 files land exactly once.
3. `apitest`, **run against `logservice-go` and made to pass before Fiber
   exists**. That is where the contract gets discovered. Then trim the
   now-duplicated cases out of `server_test.go`.
4. Scaffold `logservice-fiber` and **prove the Docker build before writing any
   handlers**. `go.mod` carries `replace … => ../logservice-go`, and Go loads a
   replacement's `go.mod` during module resolution, so the sibling must be inside
   the build context — the Dockerfile builds from the repo root with `-f`, the
   `webui-fastify` precedent. A root `.dockerignore` comes with it; there is none
   today, and the whole repo is currently the context for the webui build too.
5. The HTTP layer, with `contract_test.go` in place first so it fails loudly.
6. `cmd/logservice-fiber`.
7. CI, scripts, the compose service, container scripts, docs.
8. The differential test.

## Verification

| level | what | command |
|---|---|---|
| unit | `logservice-go` unchanged after the move | `cd logservice-go && go test -race ./...` |
| unit | concurrent `migrate.Run` is safe | the step-2 test |
| unit | Fiber-specific: panic middleware, `readBody` at exactly `maxBodyBytes`, `Addr()` after `:0`, `errorHandler` mapping | `cd logservice-fiber && go test -race ./...` |
| contract | the same assertions against both | `TestContract` in each module |
| e2e | the **unmodified** suite against Fiber | `pnpm test:e2e:api:fiber` |
| e2e | `logservice-go` still passes after promotion | `pnpm test:e2e:api` |
| differential | byte equality at the socket, both live | `pnpm test:ab:logservice` |
| containment | no third-party JSON encoder, no `apitest` in either binary | CI |
| migrations | the 26 files still match the frozen originals | the existing `cmp -s` step, unchanged — this adds no second copy |
| consumer | the console's grids render from Fiber | point `webui`'s `LOGSERVICE_URL` at `dev-logservice-fiber:3000` |
| consumer | a gateway's audit events land | bundle `GATEWAY_LOGSERVICE_URL` → 3001, then `pnpm test:e2e:smtp` |
| clean boot | both migrate, neither loses | `docker compose down -v && docker compose up -d && pnpm provision` |

**Done means** `pnpm test:ab:logservice` passes with nothing in the header
normalisation allowlist beyond `date` / `connection` / `keep-alive`. Anything
else that has to be added there is a finding for the known-differences table in
`logservice-fiber/README.md`, not a line to add quietly.

## Deliberately not done

- **No cutover.** `deploy/core/` is untouched and `ngmaibulat/logservice-go`
  remains what production runs.
- **No third-party JSON encoder.** A drop-in like `goccy/go-json` changes HTML
  escaping of `<`, `>` and `&`, which appear in `response` strings and subject
  lines, and would alter every search response relative to `logservice-go`.
  Asserted in CI.
- **No request-logging middleware.** A fleet of gateways POSTing audit events
  would flood it; `logservice-go` has none for the same reason.
- **No benchmark in this milestone.** The differential test establishes
  equivalence. Performance numbers on a service that is DB-bound and answers a
  handful of routes would measure MariaDB, and are worth taking only once
  equivalence holds.

## What was built differently

**The plan expected `logservice-fiber` not to migrate; it does, and that turned
a preference into a defect fix.** Arguing the point produced the actual finding:
`Run` reads the applied set once and records a file only after executing it, so
the `UNIQUE(name)` comment in `createTable` was wrong about what it prevents. The
lock is now the thing that prevents it, the comment says so, and
`deploy/core/upgrade.sh` is safer than it was before this milestone.

**Which way the loser of that race dies depends on the file, not on the runner.**
The comment first written here said `duplicate column name`. Running the test
without the lock showed the loser failing on the tracking table's `UNIQUE` at
`001` instead — because `001` is `CREATE TABLE IF NOT EXISTS`, so its DDL
survives a second execution and the INSERT is what fails. Only the six
non-idempotent files fail inside the Exec, and only those leave a partially
applied schema. Both outcomes are recorded.

**The contract suite gained a `KnownDifference` field, which the plan did not
anticipate needing.** Fiber answers `501` for an unrecognised HTTP method from
`router.go`'s `defaultRequestHandler` — before routing and before `ErrorHandler`
— so neither the catch-all nor the error handler can reach it. The only fix would
wrap `app.Handler()` in a fasthttp handler, but `app.Test` serves through
`app.server.ServeConn`, so that wrapper would be invisible to the very suite
meant to prove it: M16's lesson exactly. Recording the divergence in the case
beat deleting the case, which would have quietly weakened the suite for *both*
implementations.

**The suite found a real defect in the Fiber code, of the kind only it could
find.** `readBody` refuses an oversized body without reading it — that is the
point of a cap — and with `StreamRequestBody` the body is then still on the
socket. fasthttp goes on to parse the next request on that keep-alive connection,
finds the body where a request line should be, and answers *"small read buffer"*,
corrupting every subsequent request on that connection. `refuseWithoutReading`
sets `SetConnectionClose`, which is what `net/http` does for the same reason.
Deleting the call makes two contract cases fail.

**The differential test found two more that no Go test could see**, which is the
argument for having it:

- A response larger than 2048 bytes is **chunked** on `net/http` and carries a
  **`Content-Length`** on fasthttp — `net/http` buffers 2048 bytes to sniff a
  length and gives up past that, fasthttp buffers the whole response. Identical
  bytes underneath. Rather than adding the two headers to the ignore list, the
  test now asserts each response declares exactly one valid framing and that a
  declared length matches the body.
- `/api//connection` is a **307 to the cleaned path** on `ServeMux` and a
  plain-text **404** on Fiber, which does not clean paths.

**Both are recorded rather than fixed**, and neither is reachable by a caller:
all three build their paths from constants, and every client involved handles
both framings transparently.

**`dbCtx` parents off `c.RequestCtx()`, not `c.Context()`.** Fiber v3 made `Ctx`
satisfy `context.Context`, which reads as the obvious parent and is the wrong
one: `c.Context()` returns `context.Background()` unless `SetContext` was called.
`RequestCtx` is better than the plan assumed in one way and worse in another —
fasthttp closes its `Done()` on **server shutdown**, so a shutdown does interrupt
an in-flight query, but not on client disconnect, and `Deadline()` is a
documented no-op.

**The `.dockerignore` was not optional.** The plan listed it as a nicety
alongside the root-context build; without it the whole repo, `node_modules` and
`.git` included, is the context for four images rather than one.
