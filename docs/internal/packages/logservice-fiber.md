# logservice-fiber

The logservice HTTP API on **Fiber v3**, running beside `logservice-go` rather
than instead of it. Added by M23.

`logservice-go` serves the same routes over `net/http` and remains what
production runs. This exists so the two can be compared rather than argued
about. Nothing in `deploy/core/` runs it; it is a service in the dev compose
stack, published on **3001** while `logservice` keeps 3000 — and both listen on
3000 *inside* their container, so a cutover would be an image-tag edit rather
than a configuration change.

## The premise

**The two implementations differ in their HTTP layer and in nothing else.** That
is what makes comparing them mean anything, and it is enforced structurally: the
pool, the query builder and its allowlists, the row scanner, the store, the
delivery validator and the 26 migrations are **imported** from `logservice-go`,
not copied.

M23 moved six packages out of `logservice-go/internal/` to make that possible —
`db`, `query`, `rows`, `store`, `validate`, `migrate`. **`internal/api` is the
one that stayed internal**, and that boundary is the statement.

Copying them instead would have been worse in a way no test catches:
`query/fields.go` skips an unrecognised field **silently**, so an allowlist entry
forgotten in one copy yields a filter that appears to work and returns every row;
and `rows` exists solely to keep the JSON *types* identical.

The link is `replace github.com/ngmaibulat/mailgw/logservice-go => ../logservice-go`
in `logservice-fiber/go.mod`. A `replace` rather than a `go.work`, because a
workspace file is per-checkout state that would change how `mailgw-go` and
`logservice-go` resolve too; a `replace` is recorded in the module and behaves
identically in dev, CI and Docker. It does mean the **Docker build context is the
repo root** (`docker buildx build -f logservice-fiber/Dockerfile .`), since Go
reads a replacement's `go.mod` during module resolution — the `webui-fastify`
precedent. A root `.dockerignore` came with it.

Fiber never enters `logservice-go`'s `go.mod`. That containment is the whole
reason this is a separate module rather than a second `cmd/` next door:
`logservice-go` stays at one direct dependency, and the sixteen modules Fiber
costs are visible in exactly one manifest.

## How the two are kept honest

`logservice-go/apitest` is a **shared contract suite** — an exported package
importing only the standard library and `testing`, which both implementations run
in their own `internal/api/contract_test.go`. It is the specification; they are
the subjects.

It exists because "they behave the same" is a claim until something checks it,
and the stakes are unusually concrete: `mailgw-go/internal/events` treats any 4xx
as terminal and spills the audit event to disk, `mailgw-go/internal/attach` turns
any non-2xx from `/filter/md5` into SMTP 451 and defers real mail, and
`webui-fastify` maps a non-2xx to 502. A framework's default 405 or 413 where
this answers 404 or 400 destroys audit rows or defers mail.

Two design points worth knowing:

- **Requests are built with `http.NewRequest`, not `httptest.NewRequest`.**
  `app.Test` serialises with `req.Write`, a client-side operation needing a
  host; `httptest.NewRequest` builds a server-side request. The former works for
  both.
- **A `driver.Connector` fake stands in for MariaDB**, passed to `sql.OpenDB` — no
  `sql.Register`, no global state. Its Ping succeeds and every statement fails,
  which is how `/readyz`'s 200 is reached and how a handler is driven onto its
  error path without a nil pool. `go test ./...` stays offline in both modules.

`Case.KnownDifference` records a divergence rather than deleting the case, so the
assertion keeps running against the other implementation and the entry can be
removed if the upstream reason goes away.

Above it, `tests/api/logservice.differential.test.ts` (`pnpm test:ab:logservice`,
opt-in via `MAILGW_AB=1`) sends ~34 requests to both **live** services and
compares status, body and the full header set. It catches what no in-process Go
test can — header casing, transfer framing, keep-alive, path cleaning, and the
exact JSON bytes of a real result set over a real MariaDB.

## Known differences

Everything else is asserted identical. These four are recorded rather than
papered over; the reasoning is in `logservice-fiber/README.md`.

| difference | why it cannot be reconciled |
|---|---|
| an unrecognised method → `501`, not the plain-text 404 | Fiber checks the method before routing and before `ErrorHandler`; the only fix is a wrapper `app.Test` would not exercise |
| a response over 2048 bytes → `Content-Length`, not chunked | `net/http` sniffs a length in a 2048-byte buffer and chunks past it; fasthttp buffers the whole response |
| `/api//connection` → 404, not a 307 | `ServeMux` cleans paths before matching; Fiber's router does not |
| `ReadTimeout` 10s, not 30s | fasthttp has no `ReadHeaderTimeout`, so one number covers headers and body |
| a query is not cancelled on client disconnect | fasthttp's `RequestCtx.Done()` closes on server shutdown only, and `Deadline()` is a no-op |

## Two binaries migrate now

M22 left exactly one process migrating the shared schema; M23 adds a second. That
is safe only because `migrate.Run` now takes a named MariaDB advisory lock
(`logservice_migrate`) for the length of its run and re-reads the applied set
after acquiring it.

Without it, two migrators starting in the same second — which is what
`docker compose up` on a fresh volume does — both read an empty applied set and
both execute all 26 files. The `UNIQUE(name)` on the tracking table does not
prevent that; it prevents the second *recording*. Which way the loser dies
depends on the file: on idempotent DDL it reaches the INSERT and fails there,
and on one of the six non-idempotent files it fails inside the migration and
leaves the schema partially applied.

The lock is session-scoped, so a killed migrator releases it with its connection
and cannot wedge an upgrade. It also retroactively covers
`deploy/core/upgrade.sh`, where a one-off `logservice migrate` can overlap a
still-running container.

## Verifying it

```bash
cd logservice-fiber && go test -race ./...   # includes the shared contract suite
pnpm test:e2e:api:fiber                      # the UNMODIFIED e2e suite, against 3001
pnpm test:ab:logservice                      # both live, compared byte for byte
```

The gate for the milestone is `pnpm test:ab:logservice` passing with nothing in
its header-normalisation allowlist beyond `date`, `connection` and `keep-alive`.
Anything else that has to be excluded is a finding for the known-differences
table, not a line to add quietly.

See also: [logservice](./logservice), and `plans/M23-logservice-fiber.md` for the
milestone itself.
