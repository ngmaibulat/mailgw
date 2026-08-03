# M19 — A test-only build with an unauthenticated control API

**Status:** **done** (2026-08-03)  ·  **Packages:** `mailgw-go/internal/{node,testctl}`, `cmd/mailgw-go`, `cmd/mailgw-go-test`, `tests`, `deploy`  ·  **Depends on:** M5, M12, M18  ·  **Blocks:** —

> Source: the testing cost M18 left behind. M18 removed the second configuration
> source and was right to; this milestone pays the bill it left on the test
> suite, without reopening what it closed.
>
> Read [What was built differently](#what-was-built-differently) before using
> this as a description of the code. Four things came out other than as planned,
> and one of them was a defect this milestone introduced and then found.

## Goal

Give the test suite a way to configure a gateway that does not involve a human,
a browser or a console — **in a binary that is never shipped**.

The load-bearing property: **no build that can be deployed accepts configuration
from its host.** M18's standing decision is unchanged. What changes is that
there is now a *second binary*, built from the same packages, which does — and
which the release path never produces.

## What is absent

Three gaps, and the first is a defect rather than an inconvenience.

### 1. The e2e suite cannot bootstrap from a clean state

`tests/provision.ts:221` waits for the gateway to register itself. A gateway
registers when an operator walks the wizard on `:8080` and submits a Central
Management URL — and since M12 that form is behind a session, which is behind a
claim code printed to the container log. Nothing automates it.

So this sequence, which `tests/README.md` and `docs/testing/environment.md` both
present as the way to start, does not work:

```bash
docker compose down -v && docker compose up -d && pnpm provision
```

It spins for 120 s on "the gateway to register itself" and throws. The only
reason this is not noticed daily is that `mailgw_go_data` is a *named volume*:
once someone has walked the wizard by hand, the identity and the `central_url`
survive every `docker compose down` that omits `-v`. A fresh clone, a CI runner
and `down -v` all hit it.

### 2. Every config change in a test costs a console round trip

Sign in, edit a profile, deploy, wait for the WebSocket or the 15 s poll. That
is the right path for an operator and the wrong one for a matrix of SMTP
behaviours — which is why `internal/smtpsrv`'s tests build a `config.Config` by
literal and never go near a bundle.

### 3. The bring-up wiring has no test that drives it end to end

`internal/smtpsrv/contract_test.go` constructs a `Backend` literally and does its
own `net.Listen`. It never runs `cmd/mailgw-go/listeners.go`'s chain — no
allowlist `Guard`, no `Throttle`, no connection semaphore, no PROXY parser.

`docs/internal/dev/testing.md` already names this as the hazard: "Build the
subject through the wiring where the wiring is the subject — a test that
constructs a `Backend` by literal does not exercise `cmd/mailgw-go`'s bring-up,
and that gap has hidden real defects." **M16 is the proof.** M11's connection cap
passed every package test and still wrapped the accepted connection so that
go-smtp's `c.conn.(*tls.Conn)` assertion failed — costing an `implicit_tls`
listener its TLS identity and its only pre-handshake read deadline. Every M11
test built its subject directly; three of its items only took effect through the
wiring.

The reason no such test exists is structural: the whole composition root lives in
`package main` and cannot be imported.

## Scope

A second binary, `cmd/mailgw-go-test`, serving an unauthenticated JSON API for
configuring and inspecting a gateway. Getting there requires hoisting
`cmd/mailgw-go`'s composition root into an importable package — which is the
bulk of the work, and is worth doing on its own merits.

## Package structure

```
internal/node/              the composition root, moved out of package main
  node.go       Options, Node, New, Run, Close      (from main.go: serveManaged)
  gateway.go    moved from cmd/mailgw-go/gateway.go
  agent.go      moved from cmd/mailgw-go/agent.go
  listeners.go  moved from cmd/mailgw-go/listeners.go
  bundle.go     loadBundle, bundleSource + exported Loaded / LoadFor
  logger.go     newLogger
  control.go    the narrow surface testctl drives
  *_test.go     the seven existing cmd tests, `package main` -> `package node`

internal/testctl/           the test-only REST API
  doc.go server.go handlers.go server_test.go

cmd/mailgw-go/              production main — dispatch + the CLI subcommands
cmd/mailgw-go-test/         the engineering main
```

**Why the export surface stays small.** The seven existing test files move *with*
the code, so `gateway`, `agent`, `loaded` and `smtpListeners` all stay
unexported. Only what the two mains and `testctl` need is exported: `Options`,
`Node`, `New`, `Run`, `Close`, `Loaded`, `LoadFor`, and the control methods.
That is the difference between a mechanical rename and an API redesign of 1700
lines.

**Dependency direction.** The containment is one-directional: `internal/testctl`
imports `internal/node`, and neither `internal/node` nor `cmd/mailgw-go` may
import `testctl`. That is what CI checks, and it is the only direction that
matters. The `Control` interface exists for testability — `*node.Node` satisfies
it exactly, and the handler tests use a fake to reach the malformed cases a real
Node would never produce.

`internal/node` is named for the vocabulary the docs already use — "edge node",
"the shipped node is zero-configuration". `internal/gateway` would collide with
the product name and with the existing `gateway` type.

## The two seams this reuses, and does not change

- **`applyCached`** (`agent.go:458`) — "the single place a cached bundle becomes
  running configuration — shared by boot, the pull loop, the WebSocket wake-up
  and SIGHUP". Injection becomes a fifth caller and inherits apply-error
  recording, last-good retention and the status page for nothing.
- **`loadBundle`** (`bundle.go:22`) → `config.ParseBundle` → `Bundle.Config` →
  `ruleset.Compile`. Unchanged, so an injected bundle passes exactly the
  validators a deployed one does. **This is the point**: the test API is a new
  door into the same room, not a second room.

## The API

Base path `/testctl/`. JSON in, JSON out. No authentication, by design.

| Route | Body | Answers |
|---|---|---|
| `POST /testctl/config` | raw `config.Bundle` JSON | `{version_id, applied, restart_required[]}` / 400 + the compiler error |
| `POST /testctl/config/profiles` | `{server, routing, allowlist, relays, logging?, admin?, auth?}` | same |
| `GET  /testctl/config` | — | the applied bundle, version id, sha256 (unredacted) |
| `GET  /testctl/status` | — | serving, provisioned, approved, applied version, apply error, restart_required, bound listener addresses, fingerprint, gateway_uid |
| `POST /testctl/enroll` | `{central_url, insecure_tls?, ca_file?}` | `{fingerprint, gateway_uid}` |
| `POST /testctl/reset` | — | drop the cached configuration and the console URL |
| `GET  /testctl/queue` | — | every envelope in the spool |
| `POST /testctl/queue/flush` | `{uuids?}` | make envelopes due now and nudge the scheduler |

Four things that matter:

**Apply is synchronous.** The handler returns what `applyCached` returned, so a
test knows immediately whether the configuration took and, if not, why. That is
the whole value over the console path: no polling, no 15 s wait, no scraping
logs for an apply error.

**Injected bundles get negative version ids**, derived from the bundle digest.
Negative so they cannot collide with the console's positive autoincrement;
derived rather than counted so that re-injecting unchanged bytes is idempotent —
see "What was built differently".

**`GET /testctl/status` reports bound listener addresses.** That is what lets a
test ask for `127.0.0.1:0` and discover the port instead of hard-coding 2525 and
serialising the suite.

**`enroll` does not touch the claim code.** It calls `agent.Register`
(`agent.go:233`) — the same function the wizard's `POST /register` calls. M12's
claim gate protects the *HTTP admin UI*, and nothing in the registration path is
changed or weakened. Enroll is therefore not a bypass of the console: it
automates the browser step and leaves the approval, assignment and deploy
exactly where they are.

`config/profiles` is a small composer into `config.Bundle`, not a second bundle
format. It exists because `tests/provision.ts` already carries the
operator-facing texts as inline constants (`RULESET`, `ALLOWLIST`, `SERVER`), and
a test should be able to hand those same strings to a gateway.

## Containment

The API is unauthenticated and what it does is re-point a gateway. Containment is
the design, not a mitigation bolted on:

- **A separate main.** `cmd/mailgw-go` links none of it, and CI asserts so —
  `go list -deps ./cmd/mailgw-go | grep testctl` must find nothing. This is the
  same shape as M18's "Assert no configuration can come from this host" step.
- **The release path never builds it.** `pnpm docker:push` and
  `container-push.sh` are untouched. `container-build-test.sh` produces
  `ngmaibulat/mailgw-go-test:v<ver>` — a **distinct repository name, never
  `:latest`** — so no compose file can pull it by accident.
- **The address is a required flag**, `-testctl <addr>`, with no default; the
  binary refuses to start without it. Flags are acceptable *here* precisely
  because this binary is not the shipped node.
- **It says what it is**: a WARN banner on boot, and `"build":"test-only"` in
  every `/testctl/status`.

## Sequence

Three commits, each green on its own. The refactor is separated from the feature
deliberately — it lands on the most security-sensitive wiring in the module, and
a behaviour-preserving move is worth being reviewable as a move.

1. **Move, no behaviour change.** `git mv` the four files and seven test files
   into `internal/node`; `package main` → `package node`. Hoist `newLogger`,
   `opts`/`mustParse`, `loadFor`/`loaded`. `cmd/mailgw-go` keeps dispatch and the
   subcommands. No test changes except its package clause.
2. **Split construction from running.** `serveManaged` becomes `node.New` +
   `(*Node).Run` + `(*Node).Close`; add `control.go`.
3. **The test binary and the API**, plus the Dockerfile target, the build script,
   the CI assertion and the compose profile.

## Verification

```bash
cd mailgw-go
gofmt -l ./cmd ./internal && go vet ./... && go test -race ./...
go build ./cmd/mailgw-go && go build ./cmd/mailgw-go-test
go list -deps ./cmd/mailgw-go | grep testctl && exit 1   # must find nothing
```

- **Commit 1 is verified by the absence of a diff.** The seven moved test files
  may change only their `package` clause and identifier qualifiers. Anything
  else means the move was not a move.
- **The wiring test that does not exist today**, in `internal/node`: `New` →
  `Run` → `ApplyBundle` → dial the bound SMTP port → send a message. This is
  `managed_test.go`'s `TestManaged_SMTPStartsAfterFirstDeploy` without the
  `httptest` console, and the first test to drive the real chain
  (`proxyproto → Meter → tls → Guard → Throttle → Limit`) rather than a bare
  `net.Listen`.
- **`internal/testctl` is tested against a fake `Control`** — routing, JSON
  shapes, error mapping. No gateway required.
- **Bad-bundle handling is asserted, not assumed**: inject a bundle whose rules
  name an unknown relay group; confirm 400 with the compiler's message *and*
  that the previously applied configuration is still serving. The injection path
  must not weaken the fail-closed contract.
- Each new test is **confirmed by reverting the fix and watching it fail** — the
  standing rule for this repo.

End to end:

```bash
# the console path must be unaffected; this is the regression that matters
docker compose down -v && docker compose up -d && pnpm provision && pnpm test:e2e:smtp

# the new path, from a clean volume, with no human in the loop
docker compose -f docker-compose.test.yaml down -v
docker compose -f docker-compose.test.yaml up -d
curl -sf -XPOST localhost:9090/testctl/config/profiles -d @tests/fixtures/dev-profiles.json
curl -sf localhost:9090/testctl/status
SMTP_PORT=2525 bun test tests/smtp
```

Manual: `docs/testing/plans/tp-01-smoke.md` and TP-08/TP-09 must pass unchanged —
they exercise the console path, which this milestone does not touch.

## The risk that was taken

M19.1 moved ~1700 lines of the module's most security-sensitive wiring — the
listener chain, the apply path, the shutdown ordering. It is mechanical, but
"mechanical and large" is how M16's twelve defects got in.

The mitigation was the staging: a pure move, reviewable as a move, with the suite
unchanged, before anything was built on top. It held — five of the seven moved
test files changed only by identifier qualifiers, and the whole suite passed at
every commit. The one defect this milestone did introduce was in **new** code
(see below), and was caught by exercising the real binary rather than by the
suite, which is the argument for smoke-testing a milestone before calling it
done.

---

## What was built differently

### The version id is derived, not counted

The plan said "allocated descending (-1, -2, …)". A counter gives one
configuration two ids when the same bundle is injected twice, which makes "what
is this node running?" ambiguous after a re-apply and piles up rows for nothing.
The id is now the bundle's own SHA-256 folded into a negative int64, so identical
bytes upsert the same row — which is the behaviour the console already has when
an unchanged configuration is redeployed. Negative is unchanged and is what keeps
the two id spaces disjoint.

### A defect this milestone introduced, and then found

`ApplyBundle` originally recorded `desired_version_id` **before** applying,
mirroring the pull loop. That is right there and wrong here: a rejected bundle
left the pointer on a configuration that could never apply, so the next restart
booted a failure and fell back. It is now recorded only after a successful apply.

The difference is worth stating, because the two paths look identical and are
not. In the pull loop `desired_version_id` is the **console's** intent and is
authoritative even for a bundle that fails — an operator has to be able to see
what they asked for. Here the caller is a test, it already has the error in its
hand, and a landmine for the next restart buys nothing.

Found by smoke-testing the real binary, not by the suite. `TestControl_
FailedApplyDoesNotBecomeTheDesiredVersion` was then confirmed by restoring the
old ordering and watching it fail.

### testctl imports node, and that is fine

The plan asserted `internal/testctl` "never imports internal/node", treating that
as part of the containment. It is not: what protects the shipped binary is that
**`cmd/mailgw-go` does not import `testctl`**, which `go list -deps` asserts in
CI. Importing node the other way costs the shipped binary nothing, and refusing
to would have meant restating `Applied`, `Status` and `QueueEntry` in a second
place plus an adapter in `cmd/mailgw-go-test`. `*node.Node` now satisfies
`Control` exactly, and a compile-time `var _ Control = (*node.Node)(nil)` keeps
it that way.

### The Dockerfile stage order is load-bearing

Adding the engineering image as a second stage made it the **last** stage, and a
multi-stage build with no `--target` produces the last one — so a bare `docker
build .` would have handed somebody the unauthenticated image. The test stage now
sits **before** the shipped one, so the default target is still production and
reaching the other requires `--target test`, which only
`container-build-test.sh` passes.

### Two things did not move, and one moved the other way

`reportMsgAuth` and `reportRateLimits` are `check` output rather than
composition, so they stayed in `cmd/mailgw-go` — and their two tests went with
them into `cmd/mailgw-go/check_test.go`. That is the one place the "moved test
files change only their package clause" invariant does not hold, and it holds
for the other five.

`resolveSpoolDir` went the other way, from `cmd/mailgw-go/mailq.go` to
`node.SpoolDir`, because "where is the spool" is precisely the question M18
found being answered differently in two places.

### Reset cannot stop the gateway serving

The plan said reset "stops serving". It cannot: listeners, the relay table and
the spool are bound for the life of the process, which is the same constraint
`restartRequired` exists to report. Pretending otherwise would give a test a
false pass, so the response says `restart_required: true` and explains itself. In
practice a test resets and then injects, which swaps the rules and the allowlist
— what it wanted anyway.

## Deliberately not done here

- **No authentication on `/testctl`, ever.** A token would imply the API is
  something you might reasonably expose. Containment is the separate binary.
- **`docker-compose.yaml` keeps the shipped image.** Making the test build the
  dev default would leave the console provisioning path with no automated
  coverage at all — and that is the only path production uses. The overlay in
  `docker-compose.test.yaml` is opt-in.
- **No `container-push-test.sh`.** If the engineering image ever needs to leave
  the machine that built it, that is a decision worth making on purpose.
- **The CLI subcommands are not in the test binary.** `check`, `mailq` and
  `events` stay in `cmd/mailgw-go`; `testctl` answers the same questions over
  HTTP.
- **The wizard did not get a non-interactive mode.** That would be a second
  provisioning path in the shipped binary; `enroll` achieves the same thing in a
  binary that is not shipped.
