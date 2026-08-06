# logservice-fiber

The logservice HTTP API on **Fiber v3**, running beside `logservice-go` rather
than instead of it.

`logservice-go` serves the same routes over `net/http` and remains what
production runs. This exists so the two can be compared rather than argued
about. It is judged by two things: whether `logservice-go/apitest` — one suite,
run by both — passes here, and whether the differential test finds any
difference on the wire.

```bash
go build ./... && go vet ./... && go test -race ./...
go run ./cmd/logservice-fiber            # migrate, then serve
go run ./cmd/logservice-fiber migrate    # apply pending migrations and exit
go run ./cmd/logservice-fiber version

pnpm test:logservice-fiber               # from the repo root
./bump.sh && ./container-build.sh        # the bump is SEPARATE from the build
```

## Everything below HTTP is imported, not copied

The pool, the query builder and its allowlists, the row scanner, the store, the
delivery validator and the 26 migrations come from `logservice-go` — one source,
reached through `replace github.com/ngmaibulat/mailgw/logservice-go => ../logservice-go`.

**The HTTP layer is the only thing the two implementations do not have in
common.** That is what makes comparing them mean anything. It is also why
`logservice-go/internal/api` is the one package that stayed internal there while
six others moved out.

Copying those packages instead would have been worse in a way no test catches:
`query/fields.go` skips an unrecognised field **silently**, so an allowlist entry
forgotten in one copy yields a filter that appears to work and returns every row;
and `rows` exists solely to keep the JSON *types* identical.

## The build context is the repo root

`go.mod` carries a `replace` pointing at the sibling module, and Go reads a
replacement's `go.mod` during module resolution — so `../logservice-go` has to be
inside the Docker context:

```bash
docker buildx build -f logservice-fiber/Dockerfile .   # note the "."
```

`container-build.sh` does this for you. The precedent is `webui-fastify`, which
builds from the root because it needs the workspace lockfile.

## Where Fiber's defaults had to be defeated

Each of these is byte-asserted by the contract suite, and Fiber's default
violates it. The reasoning lives at the point of defeat in `internal/api`; this
is the index.

| behaviour | Fiber's default | how |
|---|---|---|
| wrong method on a known path → plain-text 404 | 405 + `Allow` | catch-all `app.Use` registered **last**, plus `ErrorHandler` mapping 404/405/501 |
| oversized body → 400 (500 on `/filter/md5`) | 413 from `BodyLimit` | hand-rolled `readBody`; `BodyLimit` is only an 8 MiB memory backstop |
| `Content-Type: application/json`, no charset | varies by helper | never call `c.JSON`; `writeJSON` sets it and uses `encoding/json` |
| JSON body ends with `\n` | no newline | same |
| trailing slash and case do not match | both fold | `StrictRouting: true`, `CaseSensitive: true` |
| empty `API_KEY` accepts everything | `keyauth` is fail-closed | hand-written `auth`, `crypto/subtle` |
| panic → the `{"status":"Error"}` envelope | `recover`'s own body | hand-written `recoverPanics` |
| no `Server` header | none unless configured | `ServerHeader` left unset |

`SkipUnmatchedRoutes` must stay **off** (its default): it answers 404/405 *before*
the middleware chain, which would bypass the catch-all and restore the 405.

## Known differences

Everything else is asserted identical. These three are not, and each is recorded
rather than papered over.

**An unrecognised HTTP method answers `501 Not Implemented`, where
`logservice-go` answers the plain-text 404.** Fiber checks the method in
`router.go`'s `defaultRequestHandler`, *before* routing and before `ErrorHandler`
runs, so neither the catch-all nor the error handler can reach it. The only fix
would be wrapping `app.Handler()` in a fasthttp handler of our own — but
`app.Test` serves through `app.server.ServeConn`, so that wrapper would be
invisible to the contract suite, and untested wiring is what M16 was about. No
caller sends an unrecognised method: two are Go clients using GET and POST, the
third is the console using GET. Carried as a `KnownDifference` in
`apitest.Cases()` so the case still runs against `net/http`.

**`ReadTimeout` is 10s here and 30s next door.** `net/http` splits the budget —
`ReadHeaderTimeout` 10s for headers, `ReadTimeout` 30s for headers plus body —
and fasthttp has no header-only timeout, so one number has to serve for both.
10s is the stricter reading: the largest legitimate body is capped at 1 MiB, and
leaving it at 30s would bound slowloris three times looser than the service
beside this one. Not on the wire.

**A large response is framed with `Content-Length` here and
`Transfer-Encoding: chunked` next door.** `net/http` buffers 2048 bytes to sniff
a length and switches to chunked once a body outgrows that; fasthttp buffers the
whole response and can always declare one. A search returning five rows is
chunked on `logservice-go` and `Content-Length: 2254` here, with identical bytes
underneath. Both are valid HTTP/1.1 and every caller uses a client that handles
both transparently — `net/http` in mailgw-go, undici in the console, curl in
`deploy/core/deploy.sh`. The differential test does not ignore these two headers:
it checks each response declares exactly one valid framing and that a declared
length matches the body.

**A doubled slash — `/api//connection` — is a 307 to the cleaned path on
`logservice-go` and a plain-text 404 here.** `ServeMux` cleans paths before
matching and redirects; Fiber's router does not. Nothing generates such a URL:
all three callers build their paths from constants.

**A query is not cancelled when the client disconnects.** `dbCtx` parents off
`c.RequestCtx()`, whose `Done()` fasthttp closes on **server shutdown** only, and
whose `Deadline()` is a documented no-op. So a shutdown does interrupt an
in-flight query, as it does next door, but a caller hanging up mid-request leaves
the query running to the 15s `queryTimeout` rather than being cut short. The
ceiling still bounds the resource; it just is not reclaimed early. (`c.Context()`
would be worse — it returns `context.Background()` unless `SetContext` was
called, so the query would be cancellable by nothing at all.)

## One thing this found in its own code

`readBody` refuses an oversized body **without reading it** — that is the point
of a cap. With `StreamRequestBody` the body is then still on the socket, and
fasthttp goes on to parse the next request on that keep-alive connection, finds
the body where a request line should be, and answers *"small read buffer"* —
corrupting every subsequent request on that connection. `refuseWithoutReading`
sets `SetConnectionClose`, which is what `net/http` does for the same reason.

The contract suite caught it. Deleting the call makes two cases fail.

## Dependencies

Fiber v3 and the sibling module, which brings `go-sql-driver/mysql`. Sixteen
modules where `logservice-go` has two — and that number is the substance of the
comparison, not an accident of it. It is contained here: **Fiber never enters
`logservice-go`'s `go.mod`**, which is the whole reason this is a separate module
rather than a second `cmd/` next door.

CI asserts two things about what reaches the binary: no third-party JSON encoder
(a drop-in changes the HTML escaping of `<`, `>` and `&`, which appear in
`response` strings and subject lines), and no `apitest` (it imports `testing`).
