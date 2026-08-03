# Standing decisions

Choices that are expensive to reverse, with the reasoning. If you are about to
change one of these, the argument against you is here.

## Dependencies

**The Go module has nine direct dependencies.** Each one was argued for
individually, and the bar is high because this binary runs as root on
internet-facing hosts.

| Dependency | Why |
|---|---|
| `emersion/go-smtp` | the SMTP server |
| `emersion/go-sasl` | SASL, both directions |
| `emersion/go-msgauth` | DKIM, DMARC and `Authentication-Results` (M14) |
| `blitiri.com.ar/go/spf` | SPF (M14) |
| `google/uuid` | identity |
| `sigs.k8s.io/yaml` | `server.yaml`, `routing.yaml` |
| `modernc.org/sqlite` | the managed-mode config cache |
| `coder/websocket` | the deploy notification channel |
| `golang.org/x/crypto` | bcrypt, for inbound AUTH |

The authoritative list is what `go.mod` requires directly:

```bash
cd mailgw-go && go list -m -f '{{if not .Indirect}}{{.Path}}{{end}}' all
```

This table said seven for a while after M14 added the last two, which is the
failure mode this page is most prone to: a number in prose that no build step
checks.

**The two M14 added were weighed against hand-rolling both.**
`emersion/go-msgauth` is the same author as `go-smtp` and its three packages
import only stdlib plus `x/crypto/ed25519`, so it pulls in no new indirect
build. `blitiri.com.ar/go/spf` the plan expected to write by hand; it was taken
because its `DNSResolver` is `*net.Resolver`-shaped, so a single map-backed stub
serves SPF, DKIM and DMARC and no test in the module touches the network.

Things deliberately **not** taken:

- **`prometheus/client_golang`** — it would be the module's largest dependency,
  and the text format for a counter is three lines. `internal/obs` is ~300 lines.
- **`pires/go-proxyproto`** — its lazy parsing model cannot work here; see
  [the mail path](/architecture/mail-path#inbound).
- **`gobwas/glob`** — two dialects are needed and neither is quite what it does.

**`modernc.org/sqlite` costs about 6 MiB.** Measured: the stripped binary went
7.8 → 13.6 MiB and `go.sum` 11 → 30 lines. Accepted because it is pure Go and
keeps `CGO_ENABLED=0` with a `distroless/static` base; a cgo driver would mean a
different base image. All SQL lives behind `internal/store`, and only `store.go`
imports the driver, so swapping it stays a one-file change.

## A gateway accepts nothing from its host

Central Management is the **only** configuration source. The gateway reads no
environment variables, takes no configuration flags, and loads no configuration
files. What it runs on is one signed bundle, cached in SQLite under `-data`.

This reverses an earlier decision recorded here, "File mode must not regress",
which kept `-config <dir>` working byte-identically because `check`, `explain`,
the sample config directories, the contract suite and the Bun end-to-end suite
all ran on it. Two configuration sources turned out to be the thing generating
defects rather than a convenience:

- the spool directory resolved differently in each mode, so the shipped edge
  node bind-mounted a directory nothing ever wrote to;
- `check`, `explain`, `mailq` and `events` with no flags read a *directory*
  while `config show` and `claim` read the *store*, so on a node upgraded from
  file mode the diagnostic commands reported a stale configuration and an empty
  queue;
- the two parsers disagreed on purpose — lax for files, strict for bundles — so
  `check` had to print which one it had used;
- `auth_pass_env` and `events.api_key_env` named environment variables that
  could only ever resolve to the empty string, and the relay case authenticated
  with an empty password while the startup warning actively recommended it.

All four disappeared with the second source rather than being fixed.

**What replaced the things file mode was carrying.** CI validates a bundle in a
Go test (`internal/config.TestBundleConfig_FixtureIsAccepted`,
`internal/node.TestLoadBundle_FixtureCompiles`) instead of running `check` over
a sample tree. The dev compose stack boots unprovisioned like a real node, and
`tests/provision.ts` drives the console to configure it before the SMTP e2e
suite runs.

**The two things that are still host state, and why they are not exceptions.**
`-data` is where the bundle is cached and `-admin` is how a node with no bundle
yet is provisioned; neither is configuration, and neither can travel in a bundle
without a chicken-and-egg. A TLS certificate and a DKIM signing key are still
operator-placed files — see [A private key never travels in a
bundle](#a-private-key-never-travels-in-a-bundle).

Verify with `pnpm test:mailgw-go`, and by grepping: `os.Getenv` must not appear
in non-test code, which CI enforces.

## The test build is a different binary, not a flag

M19 added `cmd/mailgw-go-test`: the gateway plus `internal/testctl`, an
**unauthenticated** HTTP API that injects a configuration bundle, enrolls the
node with a console, inspects the queue and reports the addresses SMTP actually
bound.

This does **not** reverse the decision above, and the distinction is the whole
design. That decision is about what a *deployable* build accepts. `cmd/mailgw-go`
is unchanged, does not link `internal/testctl`, and CI asserts it with `go list
-deps`. There is no flag, no environment variable and no build tag that turns
the API on in the shipped binary, because the containment is the binary boundary
rather than a credential — a token would only imply the API is something you
might reasonably expose.

The reasons it was needed, both consequences of M18 rather than complaints about
it:

- **The e2e suite could not bootstrap from a clean state.** A gateway registers
  only after somebody submits a console URL in its wizard, and since M12 that
  form is behind a session behind a claim code in the container log. Nothing
  automated it, so `docker compose down -v && pnpm provision` waited 120s and
  threw. It went unnoticed because `mailgw_go_data` is a named volume: once
  someone had walked the wizard by hand, the identity survived every `down`
  without `-v`.
- **The bring-up wiring had no test that drove it.** Everything lived in
  `package main` and could not be imported, so every test built its subject
  directly — the exact gap that let M11's connection cap break an `implicit_tls`
  listener while passing the whole suite.

The second is why the refactor was worth doing on its own: `internal/node` is
now importable, and `internal/node/control_test.go` runs the real bring-up
including the full listener chain.

**What must stay true.** `pnpm docker:push` never builds the engineering image;
it is published under a different repository name and never as `:latest`;
`-testctl` has no default address; the shipped stage is the **last** stage in the
Dockerfile so a bare `docker build` cannot produce the test image; and
`docker-compose.yaml` keeps the shipped image, so the console provisioning path
— the only one production uses — keeps its coverage.

**Config injection is not a second configuration source.** It writes a cache row
and calls the same `applyCached` that boot, the poll loop, the WebSocket and
SIGHUP call, with the bundle bytes verbatim. A test therefore drives the same
wire format a console deploy does. Anything that re-marshalled or normalised the
body would put the suite on a document no console would produce, which is the
mistake M18 recorded when it rejected seeding MariaDB directly.

## Reload is all-or-nothing

Only the allowlist and the compiled ruleset hot-swap. On any failure the running
configuration stays in force. SIGHUP means "re-apply from the cache"; it used to
mean "re-read the files" as well, on the same code path.

## A private key never travels in a bundle

The console stores every version for ever and serves it to every gateway on the
profile, so a key placed there would be permanently retained and fleet-wide.
`tls.cert` and `tls.key` are paths on the gateway's own host.

## Counters have three units and they must not be mixed

*Message* (one transaction), *recipient* (one `RCPT`), *envelope* (one spooled
file). Every HELP string states which, because:

- `msg_accepted` is a **superset** of `msg_discarded` and `msg_quarantined` — a
  message every rule dropped was still answered `250`;
- `deliver_connfail` is **per relay** while `deliver_deferred` is **per
  envelope-attempt**;
- `conn_throttled` is a **subset** of `conn_accepted`, because the cap sits
  outside the allowlist.

Snapshot keys are a **console** contract (stored verbatim); Prometheus names are
a **dashboard** contract. Renaming either is breaking. Add a new key instead.

A golden test pins the key list, and a reflection test asserts every counter
field appears in the table exactly once.

## Gauges are omitted, not zeroed

When the spool cannot be read, the depth gauges are left out rather than reported
as `0`. A managed gateway has no spool before its first apply, and a fabricated
zero reads as "drained" when it means "unreadable".

## Over-long DATA lines are refused, not rewritten

Haraka injected `\r\n ` to fold them. That breaks DKIM signatures. The message is
refused `500 5.5.2` naming `max.line_length` instead.

## Things that look like oversights and are not

**`mail.requiretls` is declared and never populated.** Advertising `REQUIRETLS`
would be a promise about *outbound* delivery this gateway does not keep — relay
TLS is per-relay policy, and opportunistic is explicitly unauthenticated. It is
in the `unpopulated` registry so `check` says so.

**`mail.body` can never read `BINARYMIME`.** `EnableBINARYMIME` is off. It is
**not** in `unpopulated`, because that map is keyed by field name and `mail.body`
*is* populated — an entry would warn on every working `mail.body` rule. The
field's own description says the value cannot occur.

**Attachment scanning ships off**, matching the disabled Haraka plugin it
replaced. It needs a reachable endpoint and rows in a blocklist to do anything,
and turning it on changes what every message costs.

**`outbound.reuse_connections` ships off.** Turning it on changes what every
relay in the field sees — per-connection message caps, connection-keyed rate
limits — and nothing observable showed a need.

**`smtpgreeting` is not reproducible.** go-smtp owns the banner string. It would
need a small upstream patch adding a greeting hook.

## Quarantine release is CLI-only

Configuration flows one way — the console composes bundles, gateways pull them —
so a console button would need a console-to-gateway command channel that does not
exist. The local admin UI *could* grow one now that it is authenticated; the
console still cannot.

## The admin listener is plain HTTP

Decided, not implied. A self-signed pair authenticates nothing and teaches an
operator to click through a warning on the page where they type a secret.
`-admin-tls` with real paths, never in a bundle, is the tracked follow-up.

`deploy/gateway/05-firewall.sh` therefore remains **required, not optional**:
authentication made the firewall one control of two, not a spare.
