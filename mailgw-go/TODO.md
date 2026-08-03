# mailgw-go — backlog

**Milestone plans live in [`plans/`](../plans/) — one file per milestone, status
on line 3.** This file is the running backlog: the milestone index, work that
does not belong to a milestone, and the standing decisions.

M1, M2 and M3 are done: the gateway accepts, evaluates a declarative rule set per
recipient, routes, spools, delivers with retry and failover, posts audit events,
and there is a console to manage a fleet of them.

**Central management is the through-line.** M4–M6 finish it: a gateway stops
owning its configuration — it boots empty, a local wizard registers it with
Central Management (`webui-fastify`), an operator approves its fingerprint, and
everything after that is pulled from central into a local SQLite cache with
versioned deploys and rollback.

**M9 came first.** The 2026-07-29 review found four defects in already-shipped
M1/M2/M3 code — one of them a policy bypass confirmed against a running server.
The three gateway-side fixes (M9.1–M9.3) are **done**; M9.4 is folded into M4/M5
and M9.5 into M7. It is numbered M9 so nothing above it moves: milestone
*numbers* are identity, not running order.

| Milestone | Status |
|---|---|
| [M1 — gateway core: SMTP, spool, delivery, events](../plans/M1-gateway-core.md) | done |
| [M2 — the routing DSL](../plans/M2-routing-dsl.md) | done |
| [M3 — Central Management server (`webui-fastify`)](../plans/M3-central-management-server.md) | done |
| [M4 — local admin UI, wizard, registration](../plans/M4-local-admin-wizard.md) | **done** |
| [M5 — config pull, versioned apply + rollback, the zero-config node](../plans/M5-config-pull.md) | **done** |
| [M6 — fleet observability](../plans/M6-observability.md) | **done** |
| [M7 — queue completeness](../plans/M7-queue-completeness.md) | **done** |
| [M8 — parity hardening](../plans/M8-parity-hardening.md) | **done** |
| [M9 — correctness and durability fixes](../plans/M9-correctness-and-durability-fixes.md) | **done** (M9.4 with M4, M9.5 with M7) |
| [M10 — SMTP correctness and delivery-path defects](../plans/M10-smtp-correctness-fixes.md) | **done** |
| [M11 — resource bounds](../plans/M11-resource-bounds.md) | **done** |
| [M12 — admin UI authentication](../plans/M12-admin-ui-auth.md) | **done** |
| [M13 — inbound SMTP AUTH, and inbound DSN](../plans/M13-inbound-auth.md) | **done** |
| [M14 — message authentication: SPF, DKIM, DMARC](../plans/M14-message-authentication.md) | **done** |
| [M15 — rate limiting](../plans/M15-rate-limiting.md) | **done** |
| [M16 — fixes from the M11 re-audit](../plans/M16-m11-reaudit-fixes.md) | **done** |
| [M17 — outbound bounds that need a policy first](../plans/M17-outbound-bounds-policy.md) | planned |
| [M18 — zero configuration, enforced: removing the second source](../plans/M18-zero-config-audit.md) | **done** |
| [M19 — a test-only build with an unauthenticated control API](../plans/M19-test-only-control-api.md) | **done** |

Order worked: **M9 → M4 → M5 → M6 → M7 → M8 — M1–M9 are all done.** M9.4 landed
with M4 and M9.5 with M7.

**M10–M15 come from the audit of 2026-08-01**, which read the shipped code paths
rather than this backlog. Order worked: **M10 → M11 → M16 → M12 → M13 → M14 →
M15 → M18**, and **every milestone in `plans/` is done except M17**. M12 was the
first item in this file, promoted out of it; M13–M15 are the Deferred list,
promoted out of it. **M17 is the two questions M16 deferred** — a global cap on
pooled connections and negative caching for MX failures — both of which need a
policy decided before a number can be picked. Everything below this table is
what is left of the running backlog; a new milestone takes the next free number,
which is **M20**.

**M19 pays the bill M18 left on the test suite.** Removing the second
configuration source was right, and it left the e2e suite unable to bootstrap
from a clean state: `pnpm provision` waits for the gateway to register itself,
but registration only happens after an operator walks the wizard behind M12's
claim code, and nothing automates that — so `docker compose down -v && pnpm
start` spins for 120 s and throws. M19 adds a **second binary**,
`cmd/mailgw-go-test`, serving an unauthenticated `/testctl` API that injects a
bundle straight into `applyCached` and answers synchronously. It is never
shipped, never built by `pnpm docker:push`, and CI asserts `cmd/mailgw-go` does
not link it — so the standing decision is untouched: no *deployable* build takes
configuration from its host. Getting there hoists the composition root out of
`package main` into `internal/node`, which is what finally makes the listener
chain testable — the gap that hid M11's connection-cap defect until M16.

**M18 removed the second configuration source.** An audit against "zero CLI
args, zero env reliance" returned eleven findings, four of which were the same
defect seen from four directions — the worst being a relay `auth_pass_env` that
authenticated with an **empty password** while `check` printed "prefer
auth_pass_env". `-config`, the directory loader, both sample config directories
and both `os.Getenv` call sites are gone. **Six of its findings are still open**
and are listed in the milestone's own "Findings NOT addressed here"; the first,
`gateway.warn()` never running on any bundle after the first, means deploying
`allow_all: true` logs no open-relay warning at all.

**M10 fixed six defects in paths that carry live mail.** An oversize message,
an over-long line and an over-wide header block are now permanent refusals
instead of a `451` the sender retried for four days; `max.received_headers` and
`max.header_lines` went from dead config to the RFC 5321 §6.3 hop limit;
opportunistic outbound TLS encrypts against a self-signed relay instead of
failing the attempt, and a real downgrade is counted and logged rather than
silent; the attachment scan runs on the serve context instead of
`context.Background()`; a connection that never reaches DATA finally produces a
`Connection` row; and `internal/proxyproto` (v1 + v2, stdlib only) lets the
allowlist see the real client behind an L4 balancer. Five of its own planning
assumptions turned out to be wrong — see the milestone's "What was built
differently".

**M11 put ceilings where there were none.** Inbound now has `max.connections` —
a process-wide cap answering `421 4.7.0` and closing, wrapped *outside* the
allowlist so a flood of unlisted peers cannot fill it and throttle real senders.
The config bundle decode is bounded, body and drain alike, so a broken or hostile
console cannot exhaust memory. `failed-events/rejected/` gained a gauge and a
30-day retention sweep, the MX cache evicts, and `events_dropped` finally exposes
the one form of audit loss that is unrecoverable. Two delivery traps went with
them: context cancellation now reaches a **reused** relay connection (it could
not, so an in-flight DATA outlived `shutdown_timeout` by up to ten minutes), and
an MX resolution failure now reaches the envelope instead of leaving `mailq` and
the eventual DSN with a stale or blank reason.

**M16 is the re-audit of M11**, run over its own code before any of it was
committed, on the premise that a green test run proves nothing. It found twelve
defects, the worst of them M11's own: the connection cap wrapped the accepted
connection, which hid the `*tls.Conn` from go-smtp and dropped the only
pre-handshake read deadline the server has — so on an `implicit_tls` listener a
peer that connected and said nothing **held its semaphore slot for ever**, and
that listener also lost its TLS identity in the rules, the Received header and
the audit row. The cap is now two listeners, `Meter` under TLS and `Limit`
outside `Guard`, keeping both invariants. Alongside it: a cancellation could
close a pooled connection that another delivery had already taken, `Pool.Put`
held the pool mutex across a QUIT, the checkout probe ran outside the guard, the
rejected-events sweep deleted evidence on the pass that filed it, an abandoned
replay claim was invisible for ever, and `Send` after `Close` could panic the
process from the audit path. **What made it worth doing: every M11 test builds
its subject directly, and three of its items only take effect through
`cmd/mailgw-go`'s wiring — which had no test at all.** There is one now.

**M13 gave the gateway an inbound authorization gate that is not an IP
address.** It advertises `AUTH PLAIN` and `AUTH LOGIN` — over TLS, or in the
clear only when `tls.allow_insecure_auth` says so — against bcrypt hashes the
console issues and the bundle carries, and the credential set hot-swaps on
deploy so a revocation lands on the next AUTH command rather than the next
restart. `auth.user`, `auth.mechanism` and the new `auth.authenticated` are rule
fields at the **mail** stage, so *"authenticated senders may relay anywhere,
everyone else is allowlist-only"* is a policy rule an operator writes rather
than a branch in `session.Rcpt`. It also closed inbound RFC 3461 DSN: `ORCPT`
reaches `Original-Recipient`, `NOTIFY` decides who is told, `RET` chooses how
much of the message comes back, and `NOTIFY=SUCCESS` earns a "relayed" report,
because this gateway does not pass DSN parameters to the next hop and §5.2.7
makes it the boundary that answers for them. Two pre-existing defects came out
with it: `Original-Envelope-Id` was carrying this gateway's own uuid where RFC
3464 wants the sender's `ENVID`, and two notifications about one envelope
**overwrote each other's body on disk** — which a delay warning and a later
failure could already do.

**The gateway is zero-configuration, and there is no second source.** No
environment variables, no configuration flags, no configuration files: the image
runs with no command at all, generates its identity, serves the wizard, and takes
everything else from Central Management into a local SQLite cache. M5 left
`-config <dir>` in place for `check`, `explain`, `testdata/config`, the contract
suite and the Bun SMTP e2e; **M18 removed it**, along with the sample config
directories and both `os.Getenv` call sites, after the second source turned out
to be generating defects rather than convenience. CI now validates a bundle in a
Go test, and `pnpm provision` configures the dev stack through the console.

**M6 made the fleet legible.** Every stage of the mail path is counted
(`internal/obs`), the counters ride the existing 15-second heartbeat to the
console, `/metrics` and `/readyz` sit beside `/healthz` on the admin listener,
and every audit row now carries the `gateway` that wrote it — plus, on a
delivery, the `route_rule` that sent it there.

**M7 finished the queue.** A failed message now tells its sender: RFC 3464
bounces on permanent rejection and on `max_lifetime` expiry, a delay warning
while it is still being retried, and never a bounce for a bounce. `mailgw-go
mailq` inspects the spool and can `flush`, `rm`, `release` and `hold`, which
finally gives `quarantine/` a way out. `use_mx` resolves a relay's exchange as a
domain, and `outbound.reuse_connections` keeps relay connections open between
envelopes — shipped **off**, because turning it on changes what every relay in
the field sees. Shutdown is an ordered teardown under one
`server.shutdown_timeout` (M9.5), measured at 53 ms idle rather than always 10 s.

**M8 closed the parity gaps.** Attachments are walked and their decoded digests
checked against logservice's blocklist, with both `AttachChecker.js` bypasses
fixed — an inline part that names a file is now an attachment, and a malformed
MIME structure is a scan failure rather than an "allow". Inbound STARTTLS and
`implicit_tls` work, with a self-signed pair generated into the data directory
when a managed node has no certificate and an mtime-watching reloader so a
renewal needs no restart. Spilled audit events replay themselves on
`events.replay_interval` and on demand via `mailgw-go events`. **With this,
nothing Haraka does is missing here.**

## Follow-ups M4 and M5 created

- **The admin UI is authenticated** — the item that headed this file, closed by
  **[M12](../plans/M12-admin-ui-auth.md)**. A managed gateway mints a claim code
  on first boot and logs it; presenting it in the wizard mints a session, and
  every state-changing request carries that session and a CSRF token.
  `/metrics` and `/readyz` take a bearer token from `admin.metrics_token`
  instead, because a scraper has no browser to sign in with; unset means open.
  `mailgw-go claim reset` is the recovery path. The code is deliberately **not**
  single-use — see the milestone's "What was built differently".
  `deploy/gateway/05-firewall.sh` remains required: what an authenticated caller
  can do is re-point the gateway at a hostile Central Manager and be handed its
  relay credentials, so narrowing who can reach a root process is still worth
  doing.
- [ ] **Serve the admin UI over TLS.** Decided *against* in M12 and worth
      revisiting with a real certificate: a self-signed pair authenticates
      nothing and teaches an operator to click through a warning on the page
      where they type a secret. The shape is `-admin-tls <cert>,<key>`, paths on
      the gateway's own filesystem and never in a bundle (same rule as
      `internal/config/config.go`'s TLS comment); `internal/tlsx.NewReloader`
      already handles renewal in place, and the session cookie's `Secure` flag
      already follows `r.TLS`, so it costs a listener and a flag.
- [ ] **Re-register after an operator forgets a gateway.** The gateway still
      holds a `gateway_uid` the console no longer knows, so every signed call
      401s. `central.ErrUnknownGateway` already classifies this distinctly and
      it is logged as a warning; it should fall back to `Register` with the
      existing key rather than needing its data volume wiped.
- [ ] **A listener bind failure on the first apply is terminal for SMTP** in
      that process: `smtpListeners` is guarded by a `sync.Once` that has already
      fired, so a later bundle cannot retry the bind. It is logged at ERROR and
      the operator must restart. Making `smtpListeners` re-startable would let a
      corrected `listen:` address apply without one.
- [ ] **`check -data` opens the store read-write** — it creates the directory,
      the database and any pending migration. Running it as a different user
      than the gateway can leave stray root-owned WAL files behind. A
      `store.OpenReadOnly` would fix it.
- [ ] **`smtpsrv.Backend.Cfg` is captured at bring-up and read live** for the
      Received header hostname and the logservice event URLs. `restartRequired`
      lists `hostname` and `logging` because of it, so nothing is silently
      wrong — but putting `Backend.Cfg` behind an `atomic.Pointer` would let
      both hot-swap instead. M8 added a third reader: the attachment scanner and
      `attach.on_block` come off the same captured `Cfg`, which is why `attach`
      is on the restart list too.

## Follow-ups M8 created

- [ ] **A managed node's self-signed certificate is not a real certificate.** It
      makes the session encrypted, which is the point, but any peer that
      verifies names will refuse it. There is no ACME client and no way for the
      console to ship one; an operator wanting a real certificate must place it
      in the data directory by hand and name it in a server profile. The
      reloader means renewal needs no restart, so an external certbot on the
      host is a workable answer today — it is just not automatic.
- [ ] **`internal/attach` reads the spooled body back.** Gated on
      `attach.enabled || NeedsMIME()`, so the default costs nothing, but a
      deployment with attachment rules pays a second pass over every message. A
      streaming walk (`io.TeeReader` into an `io.Pipe`) would remove it; it was
      rejected here as substantially more failure modes for a cost nobody has
      measured as a problem yet. Measure before building it.
- **`failed-events/rejected/` has no drain** — **done in
  [M11.3](../plans/M11-resource-bounds.md)**. It now has its own gauge
  (`mailgw_failed_events_rejected`) and an `events.rejected_retention` sweep,
  defaulting to 30 days. The files are still the evidence of what logservice
  refused, so the sweep logs what it removes; it rides the replay pass, so
  `events.replay_interval: 0` disables it and the gateway warns when it would.
  **M16 fixed the sweep's clock**: `rename(2)` preserves mtime, so retention was
  measured from when the event was *spilled* and an old event refused today was
  filed and deleted on the same pass. `reject()` now stamps the file, so the age
  is age-since-filed.
- **A replay claim left behind by a killed process was lost for ever** — **done
  in [M16.10](../plans/M16-m11-reaudit-fixes.md)**. Nothing released a claim
  whose holder died between the rename and the outcome: `Pending()` skips the
  name, so the row was invisible to the CLI and to every later pass, while
  `Spool.LenAll` counted it indefinitely. `Replayer.Reclaim` now returns one to
  the pending set after 15 minutes, and the claim time rides in the filename
  because the mtime is already load-bearing for `handleUnparseable`.
- **The `tls:` block appears in neither sample `server.yaml`** — **done in M10**,
      spelled out at its default values in both `config/server.yaml` and
      `testdata/config/server.yaml`, alongside the new per-listener
      `proxy_protocol` / `proxy_trusted` keys and a note that
      `max.received_headers` and `max.header_lines` are now enforced.
- [ ] **Attachment scanning is one HTTP call inside the DATA reply.** No
      retries, bounded by `attach.timeout`, and `attach.fail` decides what a
      timeout means — but a slow scanner still adds its latency to every message
      before the client is answered. A per-digest cache would help a fleet
      sending the same attachment repeatedly.

## Follow-ups M16 deferred — now [M17](../plans/M17-outbound-bounds-policy.md)

Recorded here as well as in M16's own "deliberately not done", because a
deferral visible only inside the milestone that made it is a deferral nobody
reads again. Both are **questions before they are numbers**, which is why
neither was folded into M11's resource-bounds pass.

- [ ] **No global cap on pooled connections.** `MaxPerRelay` bounds one key, and
      with `use_mx` the key set follows DNS — so the real ceiling is
      `MaxPerRelay × distinct exchangers seen within connection_idle_timeout`.
      The reaper genuinely bounds that now (M16 made
      `connection_idle_timeout: 0` an error), so this is a sharp edge rather
      than a leak. A second, global limit needs a policy for **what to evict
      when it is reached**, and that is the part nobody has decided.
      Only reachable with `outbound.reuse_connections` on, which ships off.
- [ ] **No negative caching for MX failures.** `Resolver.Hosts` caches successes
      only, so while DNS is down every envelope, on every attempt, pays a fresh
      lookup and its timeout. Adding one means deciding **how long a failure is
      believed** — too long and a domain stays unreachable after its DNS is
      fixed, too short and it buys nothing.

## Follow-ups M15 created

- [ ] **Rate limits are per gateway, not per fleet.** Ten edge nodes with a
      limit of 100/min admit 1000/min collectively. Deliberate — a shared
      counter would put a network round trip in the accept path and turn a
      management-plane outage into a mail outage, which is exactly the property
      `/readyz` was designed to avoid — but an operator sizing a limit needs to
      know to divide by the fleet size.
- [ ] **The failed-AUTH limiter cannot disconnect.** It answers `454` and the
      peer may keep trying on the same connection; every attempt past the limit
      is refused without a bcrypt comparison, which is the CPU protection that
      matters, but the socket stays open until `inactivity_timeout`. go-smtp's
      `handleAuth` gives no hook to close from inside the SASL callback. Feeding
      a tripped failure budget into `connect_per_ip`, so the peer's *next*
      connection is refused 421, would close the gap and was left out as too
      clever for a first cut.
- [ ] **Nothing rate-limits per relay on the way out.** `outbound.concurrency`
      and `per_group_connections` bound connections, not messages per second. A
      rate limit there is a separate question about what receiving relays
      tolerate, and M7 already declined to guess at that.
- **Greylisting stays out**, and not because it is hard: it needs *durable*
  triplet state surviving restarts, which is the opposite of the memory-only
  decision above, and it delays legitimate mail by design. Separate milestone,
  separate storage answer, and a poor fit for a gateway whose senders are mostly
  known.
- **DNSBL/RBL lookups stay out.** Reputation, not rate — and the allowlist makes
  them largely redundant for this deployment shape.

## Follow-ups M14 created

- [ ] **A signing key cannot be distributed from the console.** Only the
      selector, the domain and the *path* travel in a bundle; the key itself is
      a file an operator has to place on each edge node, exactly as a real TLS
      certificate is. This is the rule, not an omission — the console keeps every
      configuration version for ever and serves it to every gateway on the
      profile — and it is deliberately not worked around by generating one
      either: a self-generated key whose public half is not in DNS produces
      signatures that **fail** verification, which is worse than not signing.
      `msgauth.Keys` watches its files by mtime, so a key rotated in place needs
      no restart; what needs one is a change to *which* keys exist.
- [ ] **There is no public suffix list**, so DMARC's organizational-domain
      fallback and relaxed alignment are approximated — one label up, and
      "equal or a subdomain of". Both err towards reporting `none`/`fail` where
      a full implementation would report a pass, never the reverse. See the
      milestone's "Known approximation". A PSL would be a megabyte of data and a
      tenth dependency; revisit only if a deployment actually hits it.
- [ ] **DKIM verification re-reads the spooled body**, the same cost
      `internal/attach` pays and gated the same way, so the default is free but a
      deployment with `msgauth.dkim.enabled` makes a second pass over every
      message. The two passes are independent today; folding them into one walk
      would halve it, and is worth doing only after somebody measures it.
- **ARC is not implemented and is not planned.** It matters for forwarding,
  which is not what this gateway does. Neither is **outbound DMARC reporting**
  (`rua`/`ruf`): a reporting pipeline is its own product, and logservice already
  holds the rows if it is ever wanted.
- **MTA-STS, DANE/TLSA and TLS-RPT stay out.** They are *transport*
  authentication rather than message authentication, they belong with the M10.2
  TLS work, and none of them has a consumer yet.

## Standing decisions

- **`modernc.org/sqlite` costs about 6 MiB.** Measured at M4: the stripped
  binary went 7.8 MiB → 13.6 MiB (+77%) and `go.sum` 11 → 30 lines, pulling
  `modernc.org/{libc,mathutil,memory}` and `golang.org/x/sys`. Accepted because
  it is pure Go and keeps `CGO_ENABLED=0` + `distroless/static` unchanged; a
  cgo driver would mean a different base image. All SQL lives behind
  `internal/store`, and `store.go` is the only file importing the driver, so
  swapping to `zombiezen.com/go/sqlite` stays a one-file change if the weight
  ever stops being worth it.

## Deferred — now planned

The four items that sat here have milestones of their own:

- Inbound AUTH → **[M13](../plans/M13-inbound-auth.md)**, **done**. `auth.user`
  and `auth.mechanism` have left the `unpopulated` registry, which is what
  "done" means for that milestone — but they moved to `StageMail` on the way,
  because connect- and helo-stage policy runs inside `Backend.NewSession`,
  before a client can possibly have authenticated. `mail.requiretls` is the one
  entry left. M13 also honoured inbound RFC 3461 DSN, which had been declined
  for the same reason REQUIRETLS still is.
- DKIM signing on relay, and SPF/DKIM/DMARC verification →
  **[M14](../plans/M14-message-authentication.md)**, **done**. SPF is evaluated
  at MAIL, DKIM and DMARC at DATA, all three are recorded in an
  `Authentication-Results` header, and outbound mail is DKIM-signed from keys on
  the gateway's own disk. Every result is a **rule field** — `spf.result`,
  `spf.domain`, `dkim.result`, `dkim.domains`, `dmarc.result`, `dmarc.policy` —
  so what to do about a failure is a rule an operator writes; **nothing is
  refused that was not refused before**. Every check ships **off** and each is
  turned on either by its `msgauth:` key or, on its own, by a rule that reads its
  fields (`Ruleset.NeedsSPF` and friends, the `NeedsMIME` precedent), so a
  configuration that never mentions it pays nothing and spools byte-for-byte what
  it received.
- Per-domain rate limiting → **[M15](../plans/M15-rate-limiting.md)**, **done**,
  together with the per-IP, per-sender, per-user and failed-AUTH limits it needed
  to be useful. Five dimensions in a `ratelimit:` block, every one off by
  default, every refusal a 4xx (421 at connect, 450 in a transaction, 454 for
  AUTH) so a limit set too low costs delay rather than mail. Read **live**: a
  limit an operator cannot adjust without restarting a mail server during an
  incident is a limit they will not use, so `ratelimit` is deliberately absent
  from `restartRequired`. The counters are in memory and **per gateway** — ten
  edge nodes with a limit of 100/min admit 1000/min between them, which is the
  right trade against putting a network round trip in the accept path.

> **DB-backed routing** has left this list: M3 gave the orphaned
> `Relays`/`RelayGroups` tables their first consumer (the composed bundle), and
> M5 is the pull side.

## Deployment and CI gaps

Found by the audit of 2026-08-01 and **deliberately out of scope for M10–M15**,
which are code-only. Recorded here so they are not lost:

- **No log rotation anywhere.** The gateway logs JSON to stdout
  (`deploy/gateway/02-checkdirs.sh:3` notes there is no log dir), neither compose
  file sets a `logging:` driver with `max-size`/`max-file`, and
  `deploy/common/install-docker.sh` never writes `/etc/docker/daemon.json`.
  Docker's default `json-file` driver is unbounded — on a busy relay the
  container log grows until the disk fills.
- **No healthchecks in compose**, despite `/healthz`, `/readyz` and `/metrics`
  existing since M6. It is not free to add: the runtime image is
  `distroless/static` with no shell, no `curl` and no `wget`, and the binary has
  no `healthcheck` subcommand. Adding one is the cheap fix.
- **No resource limits** — no `mem_limit`, `cpus`, `pids_limit` or `ulimits` in
  any of the three compose files.
- **Both compose files pin `:latest`**, so `docker compose pull` is unversioned
  and an upgrade is not repeatable or rollback-able. Note this interacts with a
  real hazard: `internal/store` migrations are forward-only and `Open` refuses a
  database newer than the binary, so **rolling an image back after a schema bump
  bricks the node** until its data volume is replaced — which destroys the
  identity and forces re-approval.
- **No backup or restore tooling.** `deploy/README.md:141-147` says to back up
  `/opt/mailgw-go/data` and nothing does it; there is no restore procedure, and a
  naive `cp` of the SQLite file is unsafe in WAL mode. **The spool is never
  mentioned as backup-worthy at all**, though `/opt/mailgw-go/queue` holds
  undelivered, quarantined and dead mail. `deploy/core/upgrade.sh` re-runs
  migrations with no pre-upgrade dump.
- **CI covers only the Go module.** `.github/workflows/go.yml` runs `gofmt`,
  `vet`, `test -race`, `build` and two `check` invocations;
  `.github/workflows/publish.yml` builds the **legacy Haraka** image, not
  mailgw-go, whose image is only ever pushed by a human running
  `container-push.sh`. Nothing runs `logservice`'s tests, `webui-fastify`'s
  checks, or the Bun e2e suites. No lint beyond `vet`, no `govulncheck`, no
  coverage gate, no image scan.
- **No alerting on the counters this code documents as alarms** —
  `mailgw_dsn_unroutable_total` ("any non-zero value is a configuration problem:
  senders are not learning their mail failed") and `mailgw_events_spilled_total`
  ("non-zero means the log tables are missing rows until a replay succeeds").
  There is no Prometheus, no scrape config and no dashboard anywhere in the repo.
- ~~**`events.Stats.Dropped` is never exposed.**~~ **Done in
  [M11.7](../plans/M11-resource-bounds.md)** — `mailgw_events_dropped_total`,
  whose HELP string is explicit that a dropped event is gone where a spilled one
  is parked on disk and still replayable.

## Known gaps and decisions

- **`smtpgreeting` is not reproducible.** go-smtp owns the banner string. Would
  need a small upstream patch adding a greeting hook.
- **Over-long DATA lines are refused, not rewritten** — deliberate; Haraka's
  `\r\n ` injection breaks DKIM. M10 changed the *answer* but not this decision:
  such a line used to earn a `451`, so the sender retried a message whose format
  could never become acceptable, and it is now `500 5.5.2` naming
  `max.line_length`. Note the M10 plan assumed go-smtp answered this itself —
  true of command lines, not of DATA.
- **Quarantine release is CLI-only.** `mailgw-go mailq release <uuid>` is the
  path M7 added. The objection M12 existed to remove is gone — a release button
  on the local admin UI is now a session-and-CSRF-protected POST like any other,
  so that half is buildable. A **console** button still is not: config flows one
  way today, as bundles, so it would need a console→gateway command channel that
  does not exist. Separate work either way.
- **Plaintext relay credentials** still work for parity. `auth_pass_env` is
  supported and `check` warns; the committed mailtrap credentials under
  `deploy/mailgw/settings/mine/config/` should be rotated regardless.
- **Attachment scanning ships off** (`attach.enabled: false`), matching the
  commented-out `#ngmFilterAttach` entry in `legacy/mailgw/config/plugins`. It
  needs a reachable `/filter/md5` and rows in `BlockMD5s` to do anything, and
  turning it on changes what every message costs — the same treatment
  `outbound.reuse_connections` gets.
- **A private key never travels in a configuration bundle** (M8). The console
  stores every version forever and serves it to each gateway on the profile, so
  a key placed there would be permanently retained and fleet-wide. `tls.cert`
  and `tls.key` are paths on the gateway's own host; a managed node with neither
  generates a self-signed pair into its data directory.
- **`mail.requiretls` can never be true.** `EnableREQUIRETLS` is not set, so
  go-smtp answers `504` and no client can ask for it. Declared in `unpopulated`
  since M8 so `check` says so rather than the rule silently never matching.
  Advertising it would be a promise about outbound delivery this gateway does
  not currently keep. **M13 revisited it and kept this decision**, and closed
  its two neighbours differently. Inbound RFC 3461 DSN is now advertised and
  honoured end to end (`NOTIFY=`, `ORCPT=`, `RET=`, `ENVID=`), gated on
  `dsn.enabled` — this gateway does generate notifications, so accepting a
  sender's instructions about them is a promise it can keep. `mail.body ==
  "BINARYMIME"` was NOT added to `unpopulated`, contrary to what M13.4
  suggested: that map is keyed by field name and `mail.body` *is* populated, so
  an entry would warn on every working `mail.body` rule. The field's own
  description says the value cannot occur instead.
