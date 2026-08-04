# Testing

## What runs where

| Suite | Command | Needs |
|---|---|---|
| Go unit + contract | `cd mailgw-go && go test -race ./...` | nothing |
| logservice-go unit | `cd logservice-go && go test -race ./...` | nothing |
| logservice unit (frozen Bun) | `cd logservice && bun test ./tests/` | nothing |
| Console | `cd webui-fastify && pnpm test` | nothing |
| **Gateway e2e (tier B)** | `pnpm test:e2e:gateway` | a Go toolchain |
| **Stack e2e (tier A)** | `pnpm test:e2e:stack` | the compose stack + the engineering image |
| SMTP end-to-end | `pnpm test:e2e:smtp` | a provisioned stack |
| API end-to-end | `pnpm test:e2e:api` | a running stack |

::: warning `bun test tests/` is a FILTER, not a path
It matches every directory named `tests` in the repository, so it also runs
logservice's unit suite — one of whose files sets `process.env.API_KEY`, which
un-skips an e2e auth test that then fails against a stack with no key. Every
script now says `./tests/`. Use the leading `./` when you run it by hand.
:::

`MAILGW_API_E2E=1` and `MAILGW_DB_CHECK=1` gate the database-mutating suites.
The `pnpm test:e2e*` scripts set them — before, nothing did, so
`pnpm test:e2e:api` ran zero tests and exited 0.

## The two e2e tiers, and what decides which one a test belongs to

`internal/smtpsrv` alone has around 130 tests, each on a real socket. A test
outside Go earns its place only if it needs something Go cannot cheaply give:

| Concern | Owner |
|---|---|
| Rule semantics, TLS, AUTH, DSN rendering, limits, the listener chain | Go, in process |
| The **chain** — accept → spool → defer → flush → deliver → audit — across a **process boundary** | **Tier B**, `tests/gw/` |
| A **restart** over one data directory, a binary's flags and exit codes | **Tier B** |
| The real **image**, the **console**, MariaDB, logservice, MailHog | **Tier A**, `tests/stack/` |

Anything a Go test already asserts appears in Tier B **once**, as part of a
chain, never as a case of its own. A slower copy of `TestContract_*` is the
duplication these tiers exist to avoid.

**Tier B** (`tests/gw/`, harness in `tests/harness/`) spawns a real
`cmd/mailgw-go-test` per suite on a throwaway data directory, with a scriptable
fake relay (`harness/sink.ts`) and a fake logservice (`harness/logsink.ts`). It
needs no Docker and no network. All three listeners are asked for port **0** and
all three bound addresses come back — SMTP in `status.listeners[]`, the control
API on **stdout**, the admin UI in `status.admin_addr` — so nothing reserves a
port and nothing races.

The fake relay is what makes TP-06 and TP-07 automatable at all: MailHog accepts
everything, and TP-07's own preconditions say so ("A second MailHog will not do
this — use a small `nc`-scripted listener").

**Tier A** (`tests/stack/`) runs against the compose stack and owns what fakes
cannot prove: that a third-party MTA accepts what this gateway produces, that
the audit events survive the real logservice's validator and migrations, and
that the **console's** bundle is one the gateway accepts —
`tests/stack/bundle-contract.test.ts` is the only assertion in the repository
that `webui-fastify/src/central/bundle.ts` and `mailgw-go/internal/config`
agree.

Tier A has **one writer**: `tests/stack/console.test.ts` is the only file that
mutates configuration, and every other file is read-only with respect to it.
That is the serialisation mechanism — not a mutex, which would only legitimise a
second writer. Files converge on the baseline in `beforeAll` rather than relying
on a predecessor's `afterAll`, because the case that matters is the one where
the predecessor crashed.

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

- **CI runs the Go module and the e2e suites.** `.github/workflows/go.yml`
  covers the Go module; `e2e.yml` adds two jobs — `gw` (tier B, no Docker, on
  every push and PR) and `stack` (tier A, compose, on main and on demand).
  Nothing still runs the **logservice** tests or the **console** checks, and
  `publish.yml` builds the **legacy Haraka** image rather than the gateway.
- **No lint beyond `go vet`**, no `govulncheck`, no coverage gate, no image scan.
- **The manual test plans** cover what the automated suites cannot — a real
  relay, a real browser, a real network. See the
  **test plan site** (`docs/testing`).
