# mailgw-go

A Go rewrite of the Haraka SMTP gateway (now frozen under `legacy/mailgw/`),
built on [`emersion/go-smtp`](https://github.com/emersion/go-smtp).

**It is the gateway both compose stacks run.** It owns port 25; the Haraka
service is commented out, not deleted, so a parity run is one uncomment away.
Standalone it listens on 2525.

## Why

Two problems with the Haraka plugin set motivated it:

**Routing was limited to four fields and one operator.** The whole matcher is
`mailgw/plugins/Route.js:44` — a case-insensitive string equality over `sender`,
`sender_domain`, `rcpt` and `rcpt_domain`, ANDed together. `rcpt_domain: ngm.dev`
does not match `mail.ngm.dev`, and `npRoute.js:55` passes only `rcpt_to[0]`, so a
message to two recipients on different routes went entirely to the first
recipient's relay.

**Delivery events were being lost silently.** `npLogDelivery.js:37` sends
`rcpt_accepted` as a comma-joined list, but logservice validates it as a single
address — so every multi-recipient delivery was rejected with a 400. Because the
POST is fire-and-forget with no `response.ok` check (`functions.js:91`), nothing
reported it.

## What it does today

- **Connect-stage IP allowlist**, reading the existing `ngmfilter.json`. Now with
  CIDR and IPv6 support, and still **fail-closed**: a missing or malformed
  allowlist denies everything, exactly as `npFilter.js:52-57` does.
- **A declarative rule engine** matching on any connection, envelope or message
  fact, with per-recipient evaluation — see below.
- **Per-recipient routing**, so one message can split into several envelopes
  bound for different relay groups.
- **A durable outbound spool** with retry, backoff, per-relay-group failover and
  crash recovery.
- **One delivery event per recipient**, which is what makes the audit trail
  correct.
- **Counters for every stage of the mail path**, exposed as Prometheus text on
  the admin listener and reported to Central Management on each heartbeat.
- **Bounces that say why** — RFC 3464 delivery status notifications when a
  recipient is permanently rejected, when a message outlives `max_lifetime`, and
  when a rule refuses a recipient after the message was already accepted, plus a
  delay warning while one is still being retried. Never a bounce for a bounce.
- **A queue you can look at and act on**: `mailq` lists every envelope, and
  `flush`, `rm`, `release` and `hold` move them — including out of quarantine,
  which nothing else drains.
- **Attachment scanning**: a MIME walk that hashes each attachment's *decoded*
  content and checks it against logservice's MD5 blocklist. Off by default; it
  fails **closed**, and a part that names a file counts as an attachment however
  it is dressed.
- **Inbound TLS**: STARTTLS on the plain listener and `implicit_tls` for a
  submissions port. A managed node with no certificate of its own generates a
  self-signed pair; a certificate renewed in place is picked up without a
  restart.
- **An audit trail that catches up**: events logservice would not take are
  parked in `failed-events/` and resent on a slow timer, or on demand with
  `mailgw-go events replay`.

## The rule engine

`routing.yaml` holds two ordered lists. `policy` decides whether to accept;
`routes` decides where to send. Both use the same predicates and the same
matcher.

```yaml
version: 1
default_action: { action: tempfail, code: 451, message: "No route found" }

policy:
    - name: "reject mail loops"
      match: { field: msg.received_count, op: gt, value: 100 }
      then: [{ action: reject, code: 554, enhanced: "5.4.6", message: "Too many hops" }]

    - name: "block executables leaving the build VLAN"
      priority: 10
      match:
          all:
              - { field: conn.remote_ip, op: in_cidr, values: ["10.20.0.0/16"] }
              - { field: attachment.filename, op: glob, value: "*.{exe,scr,js,vbs}" }
              - not: { field: mail.from, op: in, values: ["build@ngm.dev", "ci@ngm.dev"] }
      then: [{ action: reject, code: 550, message: "Executable attachments are not permitted" }]

routes:
    - name: "partner subdomains over the TLS relay"
      priority: 30
      match: { field: rcpt.domain, op: glob, value: "*.partner-a.com" }
      then:
          - { action: add_header, name: "X-NGM-Route", value: "partner" }
          - { action: relay, relay: PartnerTLS }

    - name: "everything else"
      priority: 1000
      match: { always: true }
      then: [{ action: relay, relay: Outbound }]
```

**Fields** are namespaced by the stage they become known at — `conn.*`, `helo.*`,
`auth.*`, `mail.*`, `rcpt.*`, `msg.*`, `header.<name>`, `header_count.<name>`,
`attachment.*`, `tag.<key>`. Run `mailgw-go fields` for the full list with types.
Every name is checked against that registry at load time, so a typo is a startup
error rather than a rule that quietly never fires.

**Operators**: `eq ne contains prefix suffix glob regex in in_cidr lt le gt ge
exists empty`. **Combinators**: `all`, `any`, `not`, `every`, `always`. Regular
expressions are Go's RE2 — linear time, no backtracking, so no rule can hang the
gateway on a crafted address.

Three details worth knowing:

- **`glob` stops at a dot for domain-shaped fields.** `*.partner.com` matches
  `mx.partner.com` and not `partner.com`. Filenames are the opposite, so `*.exe`
  still matches `report.q3.exe`.
- **A leaf over a multi-valued field is existential.** `attachment.filename glob
  "*.exe"` means *some* attachment is one, so `not: {…}` reads as "no attachment
  is a .exe" — what a blocklist author means. `every` gives the universal form.
- **Rules evaluate as early as their fields allow.** A rule mentioning only
  `rcpt.*` fires at RCPT, so a per-recipient refusal reaches the client on its
  own `RCPT TO` line instead of failing the whole message after DATA. The walk
  stops at the first rule that needs a later stage, so an early decision is
  always the same one the final stage would reach.

`routing.json` is still read when `routing.yaml` is absent, and transpiles into
exactly these structures — `internal/ruleset/transpile_test.go` runs both
matchers over the same envelope matrix and requires identical answers, and the
whole SMTP contract suite runs through the compiled engine.

## Commands

```bash
go build ./... && go vet ./... && go test -race ./...

./mailgw-go check    # validate the cached bundle; non-zero on error
./mailgw-go serve    # run
./mailgw-go fields                             # list every matchable field
./mailgw-go convert-routing routing.json > routing.yaml
./mailgw-go explain \
    --ip 10.20.0.5 --helo box --from a@x.com --rcpt b@ngm.dev \
    [--stage connect|helo|mail|rcpt|data] [--tls] [--eml msg.eml]
./mailgw-go -version

# Managed mode: no arguments at all. This is what the container runs.
./mailgw-go
./mailgw-go config show -data /var/lib/mailgw-go   # the running bundle, redacted
./mailgw-go check -data /var/lib/mailgw-go         # is it valid?
./mailgw-go claim status -data /var/lib/mailgw-go  # the admin UI's claim code
./mailgw-go claim reset  -data /var/lib/mailgw-go  # rotate it, sign everybody out
```

### The queue

```bash
./mailgw-go mailq           # list every envelope
./mailgw-go mailq -q quarantine      # just what is held
./mailgw-go mailq -json              # for a monitoring script
./mailgw-go mailq flush              # everything ready, due now
./mailgw-go mailq flush   <uuid>...
./mailgw-go mailq release <uuid>...  # quarantine -> queue
./mailgw-go mailq hold    <uuid>...  # queue -> quarantine
./mailgw-go mailq rm      <uuid>...
```

Run it as the user the gateway runs as. It attaches to an existing spool and
deliberately will not create one: a root-owned directory beside the gateway's own
is far harder to notice than "no such spool".

An envelope being delivered right now cannot be flushed, removed or held.
Deleting its file would not cancel the delivery — the worker finishes the attempt
and rewrites the envelope — so those are refused rather than pretended.

Released mail moves without a restart. `outbound.poll_interval` is a ceiling
rather than a tick, precisely so something appearing in `q/` by another hand is
picked up on the next rescan.

## Audit events that did not land

An event that logservice refused — because it was unreachable, or because it
answered 4xx — is parked in `failed-events/` rather than lost. Mail delivery
never waits on the audit trail, so this is the price of that.

```sh
./mailgw-go events                  # what is parked, oldest first
./mailgw-go events        -all      # include rejected/
./mailgw-go events        -json     # for a monitoring script
./mailgw-go events replay           # resend everything now
./mailgw-go events rm     <file>... # give up on some
```

The gateway also replays on its own every `events.replay_interval` (default 5m;
`0` disables it), so an outage that ends repairs itself. Both paths claim each
file with a rename before sending it, so running this against a live gateway
cannot double-post a row.

A 2xx deletes the file. A **4xx is terminal** — the payload does not match the
server's schema, so an identical body cannot start passing — and it moves to
`failed-events/rejected/`, set aside rather than deleted, because it is the
evidence of what was refused. Anything else is left for the next pass, and a pass
stops after three consecutive failures rather than marching a thousand events
into a closed socket.

That directory is **swept on `events.rejected_retention`** (default 30 days; `0`
keeps everything for ever). The sweep rides the replay pass, so
`events.replay_interval: 0` disables it too, and the gateway warns at boot when
it would. Age counts from when the event was **filed** under `rejected/`, not
from when it was spilled — otherwise an old event refused today would be deleted
on the very pass that filed it. `mailgw_failed_events_rejected` reports the
depth. A claim left behind by a process killed mid-replay is returned to the
pending set after 15 minutes, so no event is stranded under a name nothing
lists.

Replay posts to the endpoints in the **current** configuration, not the ones
recorded when the event failed: a managed gateway's logservice URL arrives in a
bundle and can change.

## Inbound TLS

`tls.starttls` defaults to **on** — it is an opt-out — but it is inert without a
certificate, so a configuration that never mentions TLS behaves as it always has.

```yaml
tls:
    cert: /var/lib/mailgw-go/tls/mx.crt
    key: /var/lib/mailgw-go/tls/mx.key
    starttls: true

listen:
    - addr: "0.0.0.0:25"
    - addr: "0.0.0.0:465"
      implicit_tls: true # TLS from the first byte, no STARTTLS
```

`cert` and `key` are paths on this host. **A private key is never carried in a
configuration bundle**: the console stores every version forever and serves it to
each gateway on the profile, so a key there would be permanently retained and
fleet-wide.

A **managed** node with neither set generates a self-signed pair into
`<data>/tls/` and uses that. It is not a substitute for a real certificate — any
peer that verifies names will refuse it — it is a substitute for cleartext, and
opportunistic STARTTLS with a self-signed certificate is what nearly every
sending MTA accepts. To use a real one, drop it in the same directory and name it
in the server profile.

Certificates are re-read when the files change, so a renewal in place needs no
restart.

## Attachment scanning

Off by default. When on, the message's MIME structure is walked at end of DATA,
each attachment's **decoded** content is MD5'd, and the digests are posted to
logservice's `/filter/md5` blocklist.

```yaml
attach:
    enabled: true
    url: http://logservice:3000/filter/md5
    timeout: 3s
    fail: closed # a scanner error refuses (451) rather than relaying unscanned
    include_inline: true
    on_block: reject # reject | tempfail | quarantine | discard
```

The walk also populates `attachment.*`, `msg.has_attachment` and
`msg.mime_part_count` for the rule engine — and it runs for those alone, without
`enabled`, whenever a rule reads one. Nothing is walked when nothing would read
the result, because it costs a second pass over the message.

Two distinctions worth knowing:

- **A blocked digest and a broken scanner are different events.** A block gets
  `on_block` (default: 550). A failure — timeout, bad reply, unparseable MIME —
  gets `fail`, and `closed` answers **451**, because a scanner outage is
  temporary and not the sender's doing.
- **`on_block` is the default answer, not an override.** It applies only when no
  rule reached a terminal action, so an `accept` rule is still the whitelist and
  a rule reading `tag.attach_scan` (`allow`/`block`/`error`) can decide for
  itself.

### Flags

| Flag | Default | Applies to | Purpose |
|---|---|---|---|
| `-data` | `/var/lib/mailgw-go` | all | SQLite store: gateway identity and cached configuration |
| `-admin` | `0.0.0.0:8080` | `serve` | Admin UI bind address; `""` disables it |
| `-version` | — | all | Print the version and exit |

That is the whole command line, and the defaults are what the container runs on
— the image has no `CMD` at all. **Neither flag is configuration**: `-data` is
where a configuration is *cached* and `-admin` is how a node that has none yet
gets one, which is precisely why neither can arrive in a bundle.

There used to be a `-config <dir>` here that selected a file mode. It is gone,
along with the directory loader and the sample config directories; see
[the standing decision](../docs/internal/architecture/decisions.md).

### Environment

**None.** The gateway reads no environment variables at all, and CI fails the
build if `os.Getenv` appears in non-test code.

Two used to exist and both are instructive. `events.api_key_env` named the
variable holding the logservice credential; it now arrives in the bundle as
`logging.api_key`. A relay's `auth_pass_env` named the variable holding its
password — on a gateway with no environment that resolved to the **empty
string**, so the gateway authenticated with an empty password and the relay
answered "535 authentication failed", which points at a credential being wrong
rather than absent. It is refused at load time now, by name.

`check` is worth running before any reload: it validates the relay table, every
rule against it and against the field registry, and the allowlist; it warns
about plaintext credentials and about rules matching on facts this build does
not populate yet.

`explain` prints every rule considered, why each did or did not match, and which
one won — including whether a decision had to wait for DATA.

`SIGHUP` reloads the allowlist and the rules together, all or nothing: anything
that fails to parse or type-check leaves the running configuration untouched.
Relay changes still need a restart, and a reload that would require them is
refused with a message saying so.

## Configuration

There is one source: Central Management. The gateway boots with no configuration
at all — no environment, no arguments, no files — which is how the container
runs it. It generates an Ed25519 keypair, serves the local wizard from `-admin`,
and registers with a console where an operator approves its fingerprint.
Identity and the configuration cache live in `-data` as SQLite.

A second source used to exist: `-config <dir>` read a directory at startup and
on `SIGHUP`, and `check`, `explain`, the test suites and both compose stacks ran
on it. It is gone. The sections below describe the *shape* of a configuration,
which is unchanged — bundle keys are named for the files they replaced — but
nothing reads a file of those names.

Once approved and deployed to, it pulls a signed, versioned bundle, compiles it,
applies it and starts serving. After that:

- **The gateway is authoritative for validation.** The console does shape checks
  only. A bundle whose rules do not compile leaves the running configuration
  untouched and sends the compiler's message back as `apply_error`.
- **Rules and the allowlist hot-swap**; everything else — the relay table,
  `listen`, TLS, the spool — is applied as far as it can be and reported as
  `restart_required` with a list of exactly what changed.
- **Rollback is free.** The console repoints at an older version whose bytes are
  still cached, so nothing is fetched and what runs afterwards is byte-identical
  to what ran before.
- **A console outage is a non-event.** It boots from the cache and keeps
  relaying, then reconciles when the console returns.
- **A deploy arrives in milliseconds** over a signed WebSocket (`/agent/ws`),
  with the 15-second poll behind it as a fallback. The socket carries no state —
  it says "go and look" — so losing it costs latency and nothing else.

> A managed gateway **does not serve SMTP until a configuration applies**. A
> gateway with no allowlist would deny every peer anyway, and a listener that can
> only reject is worse than no listener: it looks healthy to a load balancer.

`check`, `explain` and `config show` all accept `-data` and read the cache, so
"what is this box actually running, and is it valid?" is answerable on the box.

### The configuration

Every one of these is a **key in the deployed bundle**, named for the Haraka
file it replaced so a configuration stays diffable against the thing it came
from. `relays.json`, `ngmfilter.json` and `logging.json` keep their Haraka
shapes unchanged.

`server.yaml` replaces `connection.ini`, `smtp.ini`, `log.ini`, `me`,
`smtpgreeting` and `host_list`. The full key reference, with the Haraka setting
each value came from, is in `docs/public/config/`.

`routing.yaml` is the rule file described above, and is optional — without it
`default_action` answers every recipient. A Haraka `routing.json` can be
transpiled into it with `mailgw-go convert-routing`, which is a migration tool:
nothing reads a `routing.json` at runtime.

`admin.json` is optional too, and holds `metrics_token` — the bearer token for
`/metrics` and `/readyz`.

## Testing

`internal/smtpsrv/contract_test.go` ports every assertion from
`tests/smtp/tests/smtp.test.ts` so the SMTP contract runs under `go test`,
without Docker. The Bun suite itself runs unmodified against the real binary:

```bash
SMTP_PORT=2525 bun test tests/smtp                    # from the repo root
MAILGW_DB_CHECK=1 SMTP_PORT=2525 bun test tests/smtp  # also checks the DB rows
```

`internal/node/control_test.go` is the one that goes through the **wiring**:
`node.New` builds the real spool, runner, SMTP server and listener chain, so a
defect that exists only in bring-up has somewhere to be caught. That was
impossible until M19 moved the composition root out of `package main`.

### The engineering build

`cmd/mailgw-go-test` is this gateway plus an **unauthenticated** HTTP control
API (`internal/testctl`) that injects a configuration bundle, enrolls the node
with a console, inspects the queue and reports the addresses SMTP actually bound.

```bash
go run ./cmd/mailgw-go-test -testctl 127.0.0.1:9090 -data /tmp/mailgw-test
curl -XPOST localhost:9090/testctl/config/profiles -d @profiles.json
curl localhost:9090/testctl/status          # serving?, and the BOUND listen addrs
```

It exists because a gateway takes its whole configuration from Central
Management, which is right for a deployment and expensive for a test: every
change costs a sign-in, a profile edit, a deploy and a wait. Worse, `pnpm
provision` could not bootstrap a clean volume at all, because registering
requires walking the wizard behind M12's claim code and nothing automated it.

Injection is **not** a second configuration source. It writes a cache row and
calls the same `applyCached` that boot, the poll loop, the WebSocket and SIGHUP
call, with the bundle bytes verbatim — so a test drives the same wire format a
console deploy does. It just answers synchronously, with the compiler's own
error as the 400 body.

**It must never be deployed.** `cmd/mailgw-go` does not link `internal/testctl`
and CI asserts it; `-testctl` has no default address; the image is
`ngmaibulat/mailgw-go-test`, never `:latest`, and `pnpm docker:push` does not
build it. Build it with `pnpm build:mailgw-go:test`.

## Observability

Three endpoints on the admin listener (`-admin`, default `0.0.0.0:8080`):

| Endpoint | Auth | Answers |
|---|---|---|
| `GET /healthz` | always open | Is the process alive? Liveness only — no store, no console. A console outage must never get the process killed, and a container runtime probing it cannot present a credential. |
| `GET /readyz` | bearer token, when one is set | Should traffic go here? Provisioned, approved, a configuration applied, and SMTP listening. |
| `GET /metrics` | bearer token, when one is set | Prometheus text. Counters are `mailgw_*_total`, plus `mailgw_build_info`, `mailgw_config_version` and the queue depths. |

The token is `admin.metrics_token`, the bundle's `admin` key, which comes from
the console's `GATEWAY_METRICS_TOKEN`. Unset means **open**, which is what every deployment
that firewalled this port already has; a scraper presents it as a bearer header,
not as a session cookie, because it has no browser to sign in with. It is read
live, so a deploy that only changes it needs no restart.

```bash
curl -s localhost:8080/readyz
curl -s localhost:8080/metrics | grep mailgw_
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/metrics   # once a token is set
```

`/readyz` does **no I/O** — it reads the snapshot the last successful poll left
behind. If readiness needed a live call to the console, one console outage would
turn the whole fleet 503 at the same moment and every load balancer would drain
them together, making a management-plane failure into a mail failure. For the
same reason a failed poll, a failed deploy of a *new* version, and a pending
restart are not conditions: the gateway is still relaying on a configuration
that works.

**Three units, and they are not interchangeable** — a *message* is one SMTP
transaction, a *recipient* is one RCPT, an *envelope* is one spooled file. One
message can hold several recipients and become several envelopes, and one
envelope is retried many times. Every metric's HELP string says which it is.
Two that surprise people:

- `mailgw_messages_accepted_total` is a **superset** of the discarded and
  quarantined counters, not a disjoint bucket — a message every rule dropped is
  still answered 250.
- `mailgw_delivery_connect_failed_total` is **per relay**, so one attempt over a
  three-relay group that is wholly unreachable adds three, while
  `mailgw_delivery_deferred_total` adds one.
- `mailgw_connections_throttled_total` is a **subset** of
  `mailgw_connections_accepted_total`: `max.connections` (default 1024) is
  applied after the allowlist, so a peer has to be permitted before it can be
  turned away for being one too many. It is deliberately that way round — a cap
  applied first would let a flood from unlisted addresses fill it and throttle
  real senders.
- `mailgw_events_dropped_total` is the one audit loss with **no file anywhere**:
  the buffer was full, or the event arrived after the pipeline closed at
  shutdown. Everything counted by `mailgw_events_spilled_total` is on disk and
  still replayable.

The same counters travel to Central Management on the existing 15-second poll —
no second ticker, no second connection — where the console stores the latest
snapshot per gateway and renders it on the fleet view.

`internal/obs` is about 300 lines and pulls in nothing:
`prometheus/client_golang` would be the largest dependency in the module, and
the text format for a counter is three lines.

> **The metrics port is the management port.** The exposition includes traffic
> volumes, queue depth and the running version. Set `admin.metrics_token` and
> restrict 8080 at the firewall, exactly as for the wizard.

## Things to know

- **The admin UI is locked by a claim code, and the firewall is still required.**
  It binds `0.0.0.0:8080` by default, because a wizard reachable only over
  loopback is useless on a headless server — the whole point is to provision a
  box that has nothing on it yet. A managed gateway mints a code on first boot
  and writes it to its log; presenting it in the wizard signs an operator in,
  and every state-changing request afterwards carries that session and a CSRF
  token. `mailgw-go claim reset` rotates the code and signs everybody out.
  Unauthenticated visitors see one page: this build's version and the gateway's
  fingerprint, which stays visible so a node can still be pre-approved in the
  console before it registers.

  The code is **not consumed** by being used. A code good for exactly one
  presentation, with a cookie as the only other credential, would leave the node
  reachable by one browser for ever — a second operator, a cleared cookie or a
  new laptop would each need filesystem access to recover. What must not reopen
  is the *unauthenticated* window, and that closes the moment a code exists.

  Restrict it to the management network anyway. What an authenticated caller can
  do is re-point the gateway at another Central Manager and have it fetch that
  manager's configuration, including relay credentials; on an edge node under
  `network_mode: host` there is no port mapping to narrow and the process runs
  as root, so `deploy/gateway/05-firewall.sh` is **required, not optional**.
  Authentication made the firewall one control of two, not a spare.

  The listener is plain HTTP, deliberately: a self-signed certificate would
  authenticate nothing and would teach an operator to click through a browser
  warning on the exact page where they type a secret. A real certificate for the
  admin listener (`-admin-tls`, paths on the gateway's own filesystem, never in
  a bundle) is the tracked follow-up.

- **The IP allowlist is the only gate by default.** With no policy rule saying
  otherwise the recipient stage accepts every address, matching `npFilter.js:73`.
  Starting with an empty allowlist is refused unless `allow_all: true` is set
  explicitly.
- **A recipient refused only at DATA gets a bounce, if the refusal is
  permanent.** That happens when a data-stage rule refuses one recipient of
  several: there is no SMTP reply left to put it in, so the sender is told by
  notification instead. Only a 5xx becomes one. A 4xx — most often the default
  action, the 451 that preserves Haraka's DENYSOFT at `npRoute.js:65` — is still
  logged at WARN, because bouncing it would turn a gap in the routing
  configuration into permanent rejections for mail that would deliver fine once
  the gap was noticed.
- **A bounce is never bounced.** A message with a null sender, or one this
  gateway generated, is buried in `dead/` rather than answered with another
  notification. `dead/` is metadata-only — `Bury` collects the body — so it
  records what happened and to whom, and there is deliberately no way to
  resurrect from it.
- **A bounce needs somewhere to go.** It is routed by the same rule engine as
  ordinary mail, against a synthetic envelope with a null sender and
  `msg.is_dsn` set, so `dsn.relay_group` only matters when no route rule claims
  it. If neither does, the notification is dropped and `dsn_unroutable` counts
  it — any non-zero value there means senders are not learning their mail
  failed. `check` warns at startup when the fallback is unset.
- **Connection reuse is off by default.** `outbound.reuse_connections` keeps
  relay connections open between envelopes. Turning it on changes what every
  relay in the field sees from this gateway — many cap messages per connection,
  many rate-limit per connection rather than per message — and none of that is
  observable from here.
- **`use_mx` is a smarthost named by domain, not direct-to-MX delivery.** It
  resolves the *relay's* exchange, never the recipient's domain. Delivering per
  recipient domain would need the session to split envelopes by domain as well,
  and carries its own TLS and reputation story.
- **A crash mid-SMTP can redeliver.** Inherent to any spooling MTA: the
  alternative is losing mail.
- **`outbound.poll_interval` is a ceiling, not a tick.** Queue filenames carry
  their due-second, so the scheduler sleeps until the next envelope is actually
  owed; a new message wakes it immediately, and a finished attempt wakes it to
  re-evaluate. The interval only bounds how long something placed in `q/` by
  hand — releasing a quarantined message, say — can sit unnoticed, so it is
  cheap to raise. Due times are truncated to the second, so work can start up
  to a second early and never late.
- **Audit rows carry the gateway that wrote them.** A managed gateway sends its
  `gateway_uid`, so a log row joins the console's record; a file-mode one sends
  `server.hostname`. Delivery rows also carry `route_rule`, the rule that chose
  that recipient's relay group — per recipient, because one envelope can hold
  recipients routed there by different rules. Both are `omitempty`, so an older
  logservice simply ignores them.
- **Counters are process-lifetime and reset on restart.** They are counters, not
  gauges: read rates from them, not totals.
- **Audit events are at-least-once and may lag.** They are shipped through a
  bounded async pipeline that spills to disk, so mail keeps flowing when
  logservice is down. This differs from Haraka's fire-and-forget, which simply
  dropped them.
- **The `smtpgreeting` text is not reproducible.** go-smtp builds the banner as
  `<domain> ESMTP Service Ready` and does not expose the string. It still
  matches what the contract suite requires.
- **Over-long DATA lines are not rewritten.** Haraka injects `\r\n ` per
  `connection.ini [max] data_line_length`, which breaks DKIM signatures and can
  corrupt MIME. This is a deliberate divergence.
