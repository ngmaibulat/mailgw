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
that constructs a `Backend` by literal does not exercise
`cmd/mailgw-go`'s bring-up — and that gap has hidden real defects. If a change
only takes effect through `cmd/mailgw-go`, the test belongs there.

**`startServerTuned(t, rules, tune)`** is the seam in `internal/smtpsrv`: it
builds a real config, a real spool, a real server on a real port, and hands you a
hook to change the configuration and the backend before it starts.

**The SMTP client in the tests is hand-rolled**, deliberately independent of
go-smtp, so the tests exercise the wire protocol rather than the library.

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
