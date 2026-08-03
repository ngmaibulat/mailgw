# Testing

## What runs where

| Suite | Command | Needs |
|---|---|---|
| Go unit + contract | `cd mailgw-go && go test -race ./...` | nothing |
| logservice unit | `cd logservice && bun test tests/` | nothing |
| Console | `cd webui-fastify && pnpm test` | nothing |
| SMTP end-to-end | `SMTP_PORT=2525 bun test tests/smtp` | a running gateway |
| API end-to-end | `pnpm test:e2e:api` | a running stack |

The database-mutating suites are opt-in: `MAILGW_API_E2E=1` and
`MAILGW_DB_CHECK=1`.

## The SMTP contract is asserted twice

`internal/smtpsrv/contract_test.go` ports every assertion from
`tests/smtp/tests/smtp.test.ts`, so the contract runs under `go test` without
Docker. The Bun suite then runs **unmodified** against the built binary.

That redundancy is the point: the Go test is fast and runs everywhere, and the
Bun test proves the real binary on a real socket still behaves the same.

**Both must pass** before a mail-path change lands.

## Go conventions

**Build the subject through the wiring where the wiring is the subject.** A test
that constructs a `Backend` by literal does not exercise the gateway's bring-up
— and that gap has hidden real defects, M11's connection cap being the one that
cost a milestone. If a change only takes effect through the composition root,
the test belongs in `internal/node`.

Since M19 that is possible: the composition root moved out of `package main`
into **`internal/node`**, so a test can call `node.New(...)` and get the real
spool, events client, delivery runner, SMTP server and the full listener chain
(`proxyproto → Meter → tls → Guard → Throttle → Limit`). `New` binds no socket,
so a test that only wants to apply a configuration does not need the admin UI
listening. `internal/node/control_test.go` is the worked example: it applies a
bundle asking for `127.0.0.1:0`, reads the bound port back out of `Status()` and
completes an SMTP transaction against it — which also removes the need for
`freeAddr()`'s reserve-and-release race in new tests.

**`startServerTuned(t, rules, tune)`** is the seam in `internal/smtpsrv`: it
builds a real config, a real spool, a real server on a real port, and hands you a
hook to change the configuration and the backend before it starts.

**The SMTP client in the tests is hand-rolled**, deliberately independent of
go-smtp, so the tests exercise the wire protocol rather than the library.

**The engineering build is for the Bun suites, not for Go tests.** Go tests use
`node.New` directly and have no reason to speak HTTP to themselves.
`cmd/mailgw-go-test` exists so `tests/` can configure a gateway without driving
console forms — and so `pnpm provision` works on a clean volume at all, since
nothing else automates the wizard step. Build it with
`pnpm build:mailgw-go:test` and run the stack with `pnpm stack:test`. It is
never shipped; see [the standing decision](/architecture/decisions).

**Counters are asserted through `Snapshot()`**, by key, so a rename breaks the
test that pins the console contract.

**Golden files are a deliberate edit, not a surprise.** `internal/dsn` pins the
whole rendered notification. Regenerate with `UPDATE_GOLDEN=1 go test ./internal/dsn`
and **read the diff** — the format is a contract with every receiving mail
system.

## Verify a test actually catches the bug

The single most useful habit in this repository:

```bash
# revert the fix, keep the test
go test ./internal/smtpsrv/ -run TestTheThing     # must FAIL
# restore the fix
go test ./internal/smtpsrv/ -run TestTheThing     # must PASS
```

A test written after the fix, that passes either way, documents an intention
rather than pinning a behaviour. Several tests in this repository were rewritten
after failing this check — and at least one comment was corrected because the
mechanism it claimed to protect turned out not to be the one doing the work.

## Console conventions

`node --test`, stubbing Drizzle by monkey-patching `db.select` and friends with a
chainable stub keyed by table identity.

Anything taking a connection parameter — `composeBundle(gatewayId, conn)` — must
be tested through that parameter, and must be asserted **not** to fall back to
the module-level `db`. A query that escapes its transaction reads uncommitted
state; `src/central.tx.test.ts` exists because that regression recurs.

## What is not covered

Be honest about this when reviewing:

- **CI runs the Go module only.** `.github/workflows/go.yml` runs gofmt, vet,
  `test -race`, build and two `check` invocations. Nothing runs the logservice
  tests, the console checks, or either end-to-end suite. `publish.yml` builds the
  **legacy Haraka** image, not the gateway.
- **No lint beyond `go vet`**, no `govulncheck`, no coverage gate, no image scan.
- **The manual test plans** cover what the automated suites cannot — a real
  relay, a real browser, a real network. See the
  **test plan site** (`docs/testing`).
