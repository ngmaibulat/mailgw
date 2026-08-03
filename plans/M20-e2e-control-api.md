# M20 — End-to-end tests driven by the test-only control API

**Status:** **done** (2026-08-03)  ·  **Packages:** `tests/`, `mailgw-go/internal/{testctl,node,adminui,store}`, `.github/workflows`, `docs/{internal,testing}`  ·  **Depends on:** M19  ·  **Blocks:** —

> Source: M19 built a control API and gave it one consumer. This milestone makes
> it the basis of an end-to-end suite, and in doing so found four defects — one
> of them in M19's own code, and two in the manual test plans.
>
> Read [What was built differently](#what-was-built-differently) and
> [The defects this found](#the-defects-this-found) before using this as a
> description of the code.

## Goal

An end-to-end suite that configures a gateway synchronously and asserts on what
only a running process, a real socket and a real stack can show — **without
re-implementing in a slower language the ~130 tests `internal/smtpsrv` already
has.**

## What was absent

M19's `/testctl` had exactly one consumer: `tests/provision.ts` called `status`
and `enroll`. **Nothing called the half of the API that exists to make tests
cheap** — `POST /config`, `/config/profiles`, `GET /queue`, `POST /queue/flush`.
M19's own verification block invokes them by `curl` against
`tests/fixtures/dev-profiles.json`, a file that did not exist.

Meanwhile the Bun suite was doing almost nothing that was not already done
faster elsewhere, and several of its scripts did not do what they said:

- All 14 tests in `tests/smtp/tests/smtp.test.ts` are ported assertion-for-
  assertion into `internal/smtpsrv/contract_test.go`.
- `MAILGW_API_E2E=1` and `MAILGW_DB_CHECK=1` gated the only suites that touch
  the database, and **no script set either**, so `pnpm test:e2e:api` ran zero
  tests and exited 0.
- `bun test tests/` is a **filter**, not a path: it also ran `logservice/tests/`,
  one of whose files sets `process.env.API_KEY`, which un-skipped an e2e auth
  test that then failed against a stack with no key.
- `docker-compose.test.yaml` pinned `v0.1.2` while `VERSION` was `0.1.3`, so
  `pnpm stack:test` asked for an image that is never pushed and never `:latest`.

## The rule that keeps this from doubling the test suite

A test outside Go earns its place only if it needs something Go cannot cheaply
give: a real process boundary, a real network hop to another program, real
durability across a restart, or the real stack.

| Concern | Owner |
|---|---|
| Rule semantics, TLS, AUTH, DSN rendering, limits, the listener chain | Go, in process |
| The **chain** — accept → spool → defer → flush → deliver → audit — across a process boundary | **Tier B** |
| A restart over one data directory; the binary's flags and exit codes | **Tier B** |
| The real image, the console, MariaDB, logservice, MailHog | **Tier A** |

Anything Go already asserts appears in Tier B **once**, as part of a chain,
never as a case of its own.

## What was built

**Gateway side** — three routes and two pieces of plumbing, each a thin adapter
over something that already existed:

| Change | Why |
|---|---|
| `POST /testctl/queue/{release,hold,remove}` | `Spool.Release`/`Hold`/`Remove` already existed; quarantine's only exit was `mailq`, a CLI this binary deliberately does not carry, so quarantine was a one-way door for anything automated. `remove` is the isolation primitive that lets a file drop mail it left undeliverable. |
| **stdout `testctl <addr>`** | `-testctl 127.0.0.1:0` is the only race-free way for a harness to take a port. stdout is otherwise unused, so it stays a machine channel; parsing the WARN log line would make a human-readable string a contract. |
| **`admin_addr` on `Status`** | `listeners[]` exists *because* a configuration may ask for port 0. The admin listener has the identical problem, and `/metrics`, `/readyz` and `/healthz` — the whole of TP-10 — are unreachable from an ephemeral-port harness without it. |

An empty `uuids` is an **error** for all three verbs, where `flush` reads it as
"everything ready": "flush the queue" is a gesture an operator makes after an
outage, "release everything ever quarantined" is not one anybody wants to make
by leaving a field out.

**`tests/harness/`** — a real `cmd/mailgw-go-test` per suite on a throwaway data
directory (`gateway.ts`), a scriptable fake relay (`sink.ts`), a fake logservice
(`logsink.ts`), bundle builders (`bundle.ts`), a typed control client
(`testctl.ts`) and an SMTP client grown to carry per-recipient replies, ESMTP
parameters, AUTH, STARTTLS and dot-stuffing (`smtp.ts`, superseding
`tests/smtp/src/smtp.ts`, which re-exports it).

**Tier B** (`tests/gw/`, 72 tests, ~30s, no Docker): `control`, `pipeline`,
`routing`, `queue`, `dsn`, `restart`.

**Tier A** (`tests/stack/`, 15 tests): `delivery` (MailHog + MariaDB, replacing
the flag-gated `smtp.e2e.test.ts`), `bundle-contract` (the only assertion
anywhere that `webui-fastify/src/central/bundle.ts` and
`mailgw-go/internal/config` agree), and `console` (deploy, refusal, rollback).
`tests/provision.ts` is now a `main()` over `tests/stack/{console,baseline}.ts`,
so the script and the suites drive the console the same way.

**CI** — `.github/workflows/e2e.yml`: `gw` on every push and PR (no Docker, ~1
minute, gates a merge) and `stack` on main and on demand, uploading compose logs
on failure.

## The defects this found

Four, and the first was found by the very first Tier-B test that delivered mail.

### 1. Injected configuration cancelled the delivery runner

`Node.ApplyBundle` passed the **HTTP request's context** to `applyCached`, whose
context governs the lifetime of the delivery runner and the failed-events
replayer. Every other caller — boot, the poll loop, the WebSocket, SIGHUP —
passes a process-lifetime context. So after the first injected configuration the
gateway accepted mail with a clean `250` and **delivered none of it**, silently.

`Node.serveCtx` now carries the process context; the caller's still gates whether
the call happens at all. Pinned by
`TestControl_ApplyBundleDoesNotTieTheGatewayToTheRequest`.

### 2. An injected version id broke every heartbeat, for ever

`applied_version_id` is a `ConfigVersions.id` and the console's schema is
`positive().nullable()`. M19's injected ids are deliberately **negative**, and
were reported anyway — so the console answered **400 to every heartbeat**, the
gateway logged "cannot reach Central Management" every 15 seconds, and its
`last_seen` froze. An enrolled node that had ever been injected into looked stale
in the fleet view for the rest of its life.

`agent.report` now sends the id only when it is positive; `null` is what the
field already documents for a node running something the console did not issue.

### 3. `pnpm provision` reported success while producing a gateway that could not relay

Two independent halves. `CtrlRelay.createHandle` validates `group_id` from the
**body** — the path parameter is only used for the redirect — and `provision.ts`
never sent it, so relay creation answered **400**. Nothing checked a response
status, so the failure surfaced only as the gateway refusing every bundle with
`relay group "Outbound" is empty`, which reads like a gateway problem and is not
one. The existence check compounded it by reading the group's *edit* page, which
never mentions a relay.

### 4. A relative `-data` path was a baffling error

The store's DSN is a `file:` URL, so a relative path becomes a URI *authority*:
`-data ./x` failed with `invalid uri authority: x`, naming neither the flag nor
the directory. The shipped node always gets an absolute path, so this only ever
bit somebody running the binary by hand or from a harness.

### And two in the manual test plans

**TP-06 step 10's quarantine rule does not quarantine.** As a *route* action,
`quarantine` resolves to no relay group, and `split()` reasons that a quarantined
envelope still needs somewhere to go if it is ever released — so it **discards**.
It must be a `policy` rule, matching a **data-stage** field, with a route rule
still resolving a group. A quarantine decided by RCPT is likewise a drop: there
is no message yet to hold.

**TP-07 step 6 says a refused notification is "buried in `dead/`".** It is not:
`dead/` is the `max_lifetime` path, and an envelope every recipient permanently
rejected is *completed*. The observable signals are
`mailgw_dsn_suppressed_total` and an empty queue.

## What was built differently

### `GET /testctl/metrics` was dropped

The plan listed it as a fourth route. Once `admin_addr` exists, `/metrics` on the
admin listener is reachable — and it is the *real* operator contract, so a test
asserting on it also pins the exposition format. A second, unversioned JSON view
of the same counters is a second thing to keep in step, and `Snapshot()`'s keys
are already pinned in Go.

### Tier B keeps `tests/smtp/tests/smtp.test.ts`

The design brief argued for deleting all 14 tests as duplicates of
`TestContract_*`. They stay: the redundancy is a standing decision this repo
wrote down on purpose ("That redundancy is the point… Both must pass"), and
reversing it is a separate conversation from adding a tier.

### The two baselines diverge by one key, deliberately

`tests/stack/baseline.ts` pins `outbound.spool_dir` because that is the path the
compose file bind-mounts; `tests/harness/bundle.ts` **omits** it so a host
process gets a per-instance directory from `node.SpoolFallback`. Sharing one
constant would give whichever tier lost the argument an unwritable spool and a
gateway that fails its first apply.

### Tier A is serialised by having one writer, not by a mutex

`tests/stack/console.test.ts` is the only file that mutates configuration. A
cross-file mutex would work and would still be wrong: it legitimises a second
writer, and the next person adds one. Files converge on the baseline in
`beforeAll` rather than trusting a predecessor's `afterAll`, because the case
that matters is the one where the predecessor crashed.

### One console session, not one per file

Bun imports every test file before running any of them, and two `signIn()` calls
in flight both come back 302 with no `Set-Cookie`. `sharedConsole()` signs in
once, which is also what an operator's browser does.

## Verification

```bash
cd mailgw-go
gofmt -l ./cmd ./internal && go vet ./... && go test -race ./...
go list -deps ./cmd/mailgw-go | grep testctl && exit 1   # must find nothing

cd .. && pnpm typecheck:tests
pnpm test:e2e:gateway                                    # 72 tests, no Docker

pnpm certs && pnpm build:mailgw-go:test && pnpm stack:test
pnpm test:e2e                                            # 115 tests across 12 files
```

Every new test was confirmed by breaking what it covers and watching it fail —
the standing rule in `docs/internal/dev/testing.md`. The four defects above each
have a test that fails when the fix is reverted.

## Deliberately not done here

- **No authentication on `/testctl`.** Unchanged from M19, and for its reasons.
- **The shipped image stays the compose default.** Making the engineering build
  the default would leave the console provisioning path — the only one
  production uses — with no automated coverage. The overlay is opt-in and the
  new suites `skipIf` when it is absent.
- **No TLS, AUTH, rate-limit or attachment suites in Tier B.**
  `internal/smtpsrv` covers each with 7–18 dedicated tests on a real socket. A
  slower copy is the duplication this milestone exists to avoid.
- **The logservice tests and the console checks still are not in CI.** Adding
  them is a separate job with its own service containers; the release checklist
  still says to run them by hand.
