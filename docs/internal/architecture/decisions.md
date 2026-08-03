# Standing decisions

Choices that are expensive to reverse, with the reasoning. If you are about to
change one of these, the argument against you is here.

## Dependencies

**The Go module has seven direct dependencies.** Each one was argued for
individually, and the bar is high because this binary runs as root on
internet-facing hosts.

| Dependency | Why |
|---|---|
| `emersion/go-smtp` | the SMTP server |
| `emersion/go-sasl` | SASL, both directions |
| `google/uuid` | identity |
| `sigs.k8s.io/yaml` | `server.yaml`, `routing.yaml` |
| `modernc.org/sqlite` | the managed-mode config cache |
| `coder/websocket` | the deploy notification channel |
| `golang.org/x/crypto` | bcrypt, for inbound AUTH |

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
`cmd/mailgw-go.TestLoadBundle_FixtureCompiles`) instead of running `check` over
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
