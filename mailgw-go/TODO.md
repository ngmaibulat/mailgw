# mailgw-go — backlog

M1 and M2 are done: the gateway accepts, evaluates a declarative rule set per
recipient, routes, spools, delivers with retry and failover, and posts audit
events. The rest, in order.

**Central management comes next.** M3–M6 are new and sit *ahead* of the
remaining queue and parity work, which shifts down to M7/M8. The goal is that a
gateway stops owning its configuration: it boots empty, a local wizard registers
it with Central Management (`webui-fastify`), an operator approves its
fingerprint, and everything after that is pulled from central into a local
SQLite cache with versioned deploys and rollback.

| Milestone | Was |
|---|---|
| M3 — Central Management server (`webui-fastify`) | new |
| M4 — local admin UI, wizard, registration | new |
| M5 — config pull, SQLite cache, versioned apply + rollback | new |
| M6 — fleet observability (metrics, heartbeat, `/healthz`, gateway column) | absorbs the metrics item from the old M6 |
| M7 — queue completeness | old "M3-M5" |
| M8 — parity hardening | old M6, minus metrics |

## M2 — the routing DSL (done)

- [x] Field schema registry with per-field stage and kind
      (`conn.*`, `helo.*`, `auth.*`, `mail.*`, `rcpt.*`, `msg.*`, `header.*`,
      `attachment.*`, `tag.*`) — `internal/ruleset/schema.go`
- [x] Predicate AST + compiler + validator (`all`/`any`/`not`/`every`/`always`;
      operators `eq ne contains prefix suffix glob regex in in_cidr lt le gt ge
      exists empty`)
- [x] Stage inference, so a rule evaluates as early as its fields allow — a
      per-recipient reject fires at RCPT, not at DATA
- [x] Actions beyond `relay`: `reject`, `tempfail`, `discard`, `quarantine`,
      `add_header`, `tag`, `accept`
- [x] `explain` subcommand, plus `fields` for the registry
- [x] `convert-routing routing.json > routing.yaml`
- [x] Hot reload of the full ruleset on `SIGHUP`, atomic swap, running
      configuration retained on validation failure
- [x] Rules stay declarative and non-Turing-complete: no loops, no user code,
      RE2 only. Hold this line.

Deliberately not done, and why:

- **fsnotify auto-reload.** A config file being saved is observed mid-write, so
  it would log a spurious error on every save even with keep-on-error. `SIGHUP`
  is the signal that actually means "I finished editing".
- **Live relay reload.** Rules reload; `relays.json` does not, because swapping
  the relay table under in-flight deliveries needs runner support. A reload that
  would require it is refused with a message rather than half-applied.
- **`conn.remote_host` (rDNS).** Left out of the registry entirely: adding it
  without the lookup would give a field that silently never matches, which is
  precisely what the registry exists to prevent.

Follow-ups the DSL created:

- [ ] A recipient refused by a *data-stage* rule is dropped with a WARN, because
      the SMTP reply is already spent. It should get a DSN — folded into M7.
- [ ] `msg.has_attachment`, `msg.mime_part_count` and `attachment.*` are in the
      registry but never populated until the MIME walk lands in M8. `check` and
      startup warn when a rule uses one; remove the warnings with the feature.
- [ ] Route decisions are not recorded in the audit events, so the log tables
      cannot answer "which rule sent this message here?". Needs a logservice
      column — pairs naturally with the `gateway` column in M6.

## M3 — Central Management server (`webui-fastify`)

Done. The console side of central management, built before any Go changes so
the gateway has something to register with.

- [x] Schema: `logservice/migrations/016`–`021` — `Gateways`, `ConfigProfiles`,
      `GatewayAssignments`, `ConfigVersions` + `ConfigDeployments`, `Users.role`,
      `Sessions`. Mirrored describe-only in `webui-fastify/db/schema.ts`.
- [x] Agent API at the root Fastify scope (`/agent/*`) — outside the cookie gate
      *and* outside the audit-log hook, so a polling fleet cannot flood `Logs`.
      `POST /register`, `GET /status`, `GET /config`, `POST /report`.
- [x] **Ed25519 request signing, no tokens.** A gateway generates its own
      keypair; registration is open and lands `pending`; an operator approves a
      *fingerprint*. Canonical string is
      `METHOD\nurl\nunix-seconds\nsha256(body)`, 300s skew, scope-local raw-body
      parser so the signature covers the exact bytes sent.
- [x] Bundle composition (`src/central/bundle.ts`): assigned profiles + relay
      groups → one JSON document whose keys mirror the config-directory files
      (`server.yaml`, `routing.yaml`, `ngmfilter.json`, `relays.json`,
      `logging.json`). Handles the shape mismatches in one place — group name is
      the top-level key, `Relays.host` → `exchange`, port INT → number.
- [x] Deploy freezes an immutable `ConfigVersions` row; rollback repoints
      `desired_version_id` at an older one rather than minting a new bundle, so
      what runs afterwards is byte-identical to what ran before. Redeploying an
      unchanged configuration re-points instead of piling up versions.
- [x] Console: `/gateways` inventory + detail (approve/reject/revoke, rename,
      assignments, deploy, version list with rollback, deploy history),
      `/config/profiles` CRUD. The `/config/routing` `notimpl` stub is gone —
      routing is now a `ruleset` profile carrying mailgw-go's `routing.yaml`.
- [x] Prerequisites that genuinely blocked a fleet console: **persistent
      sessions** (a `Sessions` table, so a restart no longer logs every operator
      out and >1 replica works) and **roles** (`requireAdmin` on approval and
      every config mutation — previously any logged-in user could approve a
      gateway and read relay credentials).

Deliberately not done, and why:

- **Validating the rule DSL in the webui.** The compiler is Go. A second
  implementation in TypeScript would be a second source of truth that can
  disagree with the gateway. The console does shape checks only; the gateway
  validates on pull, keeps its last-good config, and reports `apply_error`.
- **Encrypting `auth_pass` at rest.** Still plaintext in `Relays`. Now that the
  bundle is a real consumer this is finally designable, but it belongs with M5
  (the decrypting side) rather than ahead of it.

Known gaps:

- [ ] A signed request can be replayed inside the 300s window. The signed routes
      are idempotent reads plus a metrics report, so the worst case is a stale
      report being reapplied — a nonce table would close it if reports ever
      drive anything but display.
- [ ] `GATEWAY_LOGSERVICE_URL` is one fleet-wide value. A multi-site fleet where
      gateways reach logservice at different URLs would want it per-profile.

## M4 — local admin UI, wizard, registration (mailgw-go)

- [ ] `internal/store` — SQLite via `modernc.org/sqlite` (**pure Go**: the image
      is `distroless/static` with `CGO_ENABLED=0`, so cgo drivers are out).
      Tables: identity (privkey/pubkey/fingerprint/gateway_uid), settings
      (central URL), config cache.
- [ ] `internal/central` — signing HTTP client: `Register`, `Status`, `Config`,
      `Report`.
- [ ] `internal/adminui` — `net/http.ServeMux` + `html/template` + `embed.FS`.
      Wizard (central URL → register → show fingerprint + pending), status page,
      `/healthz`.
- [ ] `-data` (default `/var/lib/mailgw-go`) and `-admin` (default
      `127.0.0.1:8080`) flags. **Flags, not env vars** — bootstrap is the wizard.
- [ ] Three boot modes: `-config <dir>` keeps today's file mode *unchanged*
      (this is what keeps CI, `testdata/config`, `check`, `explain` and the Bun
      SMTP suite working); managed-unprovisioned serves the wizard and does
      **not** start the SMTP listeners (fail-closed); managed-provisioned reads
      the cache.
- [ ] Compose: the config mount is `:ro` and the queue volume is the only
      writable path today, so a `mailgw_go_data` volume plus `EXPOSE 8080` are
      required.

## M5 — config pull, SQLite cache, versioned apply and rollback

- [ ] Refactor `load(dir)` (`cmd/mailgw-go/main.go`) behind a `Source` interface
      — `FileSource{dir}` and `CentralSource{store}`, both returning `*loaded`.
- [ ] Byte-slice parse entry points, splitting `os.ReadFile` from parse:
      `ruleset.ParseFile`, `config.ParseAllowlist`, `config.ParseServer`.
      `relays.NewTable(map)` already exists for exactly this reason, and
      `ruleset.Compile` never touches the filesystem, so the compiled artifact
      needs no change.
- [ ] Pull loop next to `watchReload`, reusing its semantics exactly:
      all-or-nothing, keep the running configuration on failure, report
      `apply_error` upward. Only allowlist + rules hot-swap; a bundle changing
      the relay table, listeners, TLS or the spool sets `restart_required`
      rather than half-applying.
- [ ] `SIGHUP` keeps working in file mode; means "re-read the cache" in managed
      mode.
- [ ] `auth_pass` encryption at rest (AES-256-GCM under a shared key) — the
      bundle is the decrypting consumer that made this designable.

## M6 — fleet observability

- [ ] Counters in `internal/obs` (the package exists and is empty — that is its
      purpose). Today the *only* counters in the binary are `events.Stats`;
      connections accepted/denied, messages accepted/rejected/quarantined,
      envelopes queued and delivery attempts/ok/deferred/bounced all have to be
      added. Queue depth comes from the existing `Spool.Len`.
- [ ] `/metrics` (hand-rolled Prometheus text, no new dependency) and `/healthz`
      on the admin server.
- [ ] Heartbeat push to `POST /agent/report`, modelled on `events.Client`:
      bounded channel, never blocks the mail path, drops with a counter.
- [ ] `022_add_gateway_column.sql` — `gateway` column + index on `Connection`,
      `Transaction`, `Delivery`. Closes the "no gateway column in the log tables"
      gap below and is what makes per-gateway log views possible at all.
- [ ] Fleet dashboard on the console.

## M7 — queue completeness

Much of M3/M4 landed early with the runner. Also done since:

- [x] The scheduler sleeps until the next envelope is due (`Spool.ReadyAndNext`
      + `Runner.sleepFor`) instead of on a fixed tick, and a finished attempt
      nudges it. `poll_interval` is now a ceiling, so it can be raised without
      delaying retries.

What remains:

- [ ] `mailq` / `flush` / `rm` CLI over the spool (`Spool.List` already exists)
- [ ] Shutdown does not wait for `Runner.Start` to return, so the process can
      exit with an attempt in flight. `Recover()` picks it up next boot, which
      is why this is cosmetic rather than lossy — but it is one more avoidable
      redelivery.
- [ ] Body reference counting via a counter file rather than the current
      directory scan in `gcBody` — fine at present volumes, O(queue) per
      completion
- [ ] `quarantine/` has no release path. Envelopes land there and stay; moving
      one back to `q/` is currently a manual `mv`.
- [ ] DSN / bounce generation (RFC 3464), delay warning, `max_lifetime` expiry
- [ ] Never bounce a bounce: `dead/` handling is stubbed but untested
- [ ] Real MX lookup (`use_mx`) for relays that want it
- [ ] Connection pooling / reuse across envelopes to the same relay

## M8 — parity hardening

- [ ] Attachment MD5 scan. **Fix the two defects before enabling it**, not
      after: `AttachChecker.js:51` skips inline-disposition parts (a bypass),
      and `:36`/`:77` fail *open* on any scanner error (another bypass). The
      config already defaults to `fail: closed` and `include_inline: true`.
- [ ] Inbound STARTTLS using the existing `tls_*.pem`
- [ ] Replayer for `queue/failed-events/`

> `/healthz` and Prometheus metrics moved to **M6** — they are how a fleet
> console tells a healthy gateway from a wedged one, so they are no longer the
> last thing on the list.

## Deferred

- [ ] Inbound AUTH
- [ ] DKIM signing on relay (`go-msgauth`)
- [ ] SPF/DKIM/DMARC verification
- [ ] Per-domain rate limiting

> **DB-backed routing** has left this list: M3 gave the orphaned
> `Relays`/`RelayGroups` tables their first consumer (the composed bundle), and
> M5 is the pull side.

## Known gaps and decisions

- **`smtpgreeting` is not reproducible.** go-smtp owns the banner string. Would
  need a small upstream patch adding a greeting hook.
- **Over-long DATA lines are counted, not rewritten** — deliberate; Haraka's
  `\r\n ` injection breaks DKIM.
- **No gateway column in the log tables.** Haraka and mailgw-go rows are only
  distinguishable by the `X-NGM-Gateway` header on relayed mail. Scheduled for
  M6 as `022_add_gateway_column.sql`; with a fleet rather than one gateway it is
  no longer merely nice to have.
- **Plaintext relay credentials** still work for parity. `auth_pass_env` is
  supported and `check` warns; the committed mailtrap credentials under
  `deploy/mailgw/settings/mine/config/` should be rotated regardless.
