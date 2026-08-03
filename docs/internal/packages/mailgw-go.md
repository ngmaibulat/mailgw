# mailgw-go

The gateway. A Go module (`github.com/ngmaibulat/mailgw/mailgw-go`), deliberately
not a pnpm workspace member. No build step beyond `go build`, no code generation.

```bash
cd mailgw-go
go build ./... && go vet ./... && go test -race ./...
gofmt -l .                      # must print nothing
```

## Layout

| Package | What it owns |
|---|---|
| `cmd/mailgw-go` | the shipped binary: CLI dispatch and the diagnostic subcommands |
| `cmd/mailgw-go-test` | the ENGINEERING binary: the above plus `internal/testctl`. Never shipped |
| `internal/node` | the composition root: process bring-up, apply/reload, the `gateway` and `agent` structs |
| `internal/testctl` | the unauthenticated test control API. Linked only by `cmd/mailgw-go-test` |
| `internal/smtpsrv` | SMTP session, listener chain, inbound AUTH, attachment glue |
| `internal/ruleset` | the rule DSL: parse, compile, evaluate, explain |
| `internal/queue` | spool, delivery runner, bounce generation |
| `internal/deliver` | outbound SMTP client, connection pool, MX resolution |
| `internal/dsn` | RFC 3464 report rendering. Knows nothing else |
| `internal/attach` | MIME walk and digests. Knows nothing else |
| `internal/msgauth` | SPF, DKIM, DMARC, and the headers that record them. Knows nothing else |
| `internal/ratelimit` | token buckets, per keyed dimension. Injected clock, knows nothing else |
| `internal/config` | configuration structs, file loading, bundle decoding |
| `internal/relays` | the relay table |
| `internal/events` | the audit event pipeline and replayer |
| `internal/obs` | the counter registry |
| `internal/store` | SQLite: identity, settings, config cache, admin sessions |
| `internal/central` | the signing client for the console |
| `internal/adminui` | the local wizard and observability endpoints |
| `internal/proxyproto` | PROXY protocol v1 and v2 |
| `internal/tlsx` | keypair generation and hot reload |
| `internal/uuidx` | the hierarchical identity type |

## Four packages that know nothing else

`internal/dsn`, `internal/attach`, `internal/msgauth` and `internal/ratelimit`
have **no** dependency on the spool, routing, or configuration. That is what lets each be pinned by a
golden file or a fixture and shared by two callers — the session and the queue
runner for `dsn`, the session and `explain -eml` for `attach`, the session and
the runner for `msgauth`, the listener chain and the session for `ratelimit`.

`ratelimit` takes its CLOCK as a dependency for the same reason `msgauth` takes
its resolver: reading the wall clock inside a limiter is what makes window tests
flaky, so it is injected from the start rather than retrofitted. Nothing in its
test suite sleeps.

`msgauth` takes its DNS through a `Resolver` interface for the same reason, so
one map-backed stub serves SPF, DKIM and DMARC and **no test in the module
performs a real lookup**.

Keep it that way. The moment one needs a `*config.Config`, it stops being
testable in isolation and starts being a second place the mail path can break.

## Where the layering matters

`internal/config` **must not import `internal/ruleset`.** `internal/smtpsrv`
imports `config` on the session hot path, and pulling the rule compiler into
everything that reads a configuration is a layering regression.

That is why bundle assembly lives in `internal/node/bundle.go` rather than in
`internal/config`: it needs `ruleset.Compile`, so it sits above both.

**`internal/node` must not import `internal/testctl`,** and neither may
`cmd/mailgw-go`. The test control API has no authentication and what it does is
re-point a gateway; the containment is that only `cmd/mailgw-go-test` links it,
and CI asserts it with `go list -deps ./cmd/mailgw-go`. The import the other way
is fine and expected — `testctl` takes a `Control` interface that `*node.Node`
satisfies exactly. See [the standing decision](/architecture/decisions).

## The `gateway` struct

`internal/node/gateway.go` owns everything that exists exactly once per process:
the spool, the event pipeline, the delivery runner, the SMTP server, its
listeners, and the atomic pointers a session reads.

```go
allowlist  atomic.Pointer[config.Allowlist]   // hot-swapped by swap()
rules      atomic.Pointer[ruleset.Ruleset]    // hot-swapped by swap()
spool      atomic.Pointer[queue.Spool]        // set once at bring-up
adminToken atomic.Pointer[string]             // read live, not in restartRequired
authUsers  atomic.Pointer[config.Auth]        // read live, not in restartRequired
limiter    *ratelimit.Limiter                 // read live; created ONCE, only its rules swap
```

`limiter` is a plain field rather than an atomic because the pointer never
changes — replacing it per apply would hand every peer a fresh allowance
whenever an unrelated configuration change was deployed.

`mu` serialises apply against apply — the config pull loop and the signal watcher
are two goroutines and both call it.

## Testing

`internal/smtpsrv/contract_test.go` ports every assertion from the Bun SMTP suite
so the SMTP contract runs under `go test` without Docker. The Bun suite then runs
**unmodified** against the binary, which is what keeps the two honest.

`internal/deliver` tests use real go-smtp instances as fake relays.

`internal/node/control_test.go` is the one that goes through the **wiring**:
`node.New` builds the real spool, runner, server and listener chain, so a defect
that only exists in the bring-up — M11's connection cap being the case that cost
a milestone — has somewhere to be caught. `New` binds no socket, so these tests
are cheap.

See [Testing](/dev/testing).

## Known sharp edges

**A listener bind failure on the first apply is terminal** for that process:
`smtpListeners` is guarded by a `sync.Once` that has already fired, so a later
bundle cannot retry the bind. Logged at ERROR; the operator must restart.

**`check -data` opens the store read-write** — it creates the directory, the
database and any pending migration. Running it as a different user than the
gateway can leave stray root-owned WAL files.

**`smtpsrv.Backend.Cfg` is captured at bring-up and read live** for the
`Received:` hostname, the event URLs and the attachment scanner. That is why
`hostname`, `logging` and `attach` are on the restart list. An atomic pointer
would let all three hot-swap.

**`Pool.totalLocked` walks the map on every `Put`.** Cheap only because `setIdle`
drops a key when it empties, so the key count is itself bounded by
`max_pooled_connections` — a few hundred adds against a network round trip. If
the cap were ever removed, this becomes an unbounded scan on the delivery path.

**Rolling an image back after a store schema bump bricks the node.** `store.Open`
refuses a database newer than the binary, and the migrations are forward-only.
Recovery means replacing the data volume — which destroys the identity and forces
re-approval.
