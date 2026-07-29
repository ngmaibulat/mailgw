# mailgw-go — backlog

M1 and M2 are done: the gateway accepts, evaluates a declarative rule set per
recipient, routes, spools, delivers with retry and failover, and posts audit
events. The rest, in order.

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
      the SMTP reply is already spent. It should get a DSN — folded into M5.
- [ ] `msg.has_attachment`, `msg.mime_part_count` and `attachment.*` are in the
      registry but never populated until the MIME walk lands in M6. `check` and
      startup warn when a rule uses one; remove the warnings with the feature.
- [ ] Route decisions are not recorded in the audit events, so the log tables
      cannot answer "which rule sent this message here?". Needs a logservice
      column.

## M3-M5 — queue completeness

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

## M6 — parity hardening

- [ ] Attachment MD5 scan. **Fix the two defects before enabling it**, not
      after: `AttachChecker.js:51` skips inline-disposition parts (a bypass),
      and `:36`/`:77` fail *open* on any scanner error (another bypass). The
      config already defaults to `fail: closed` and `include_inline: true`.
- [ ] Inbound STARTTLS using the existing `tls_*.pem`
- [ ] `/healthz` and Prometheus metrics (the counters exist in `events.Stats`)
- [ ] Replayer for `queue/failed-events/`

## Deferred

- [ ] Inbound AUTH
- [ ] DKIM signing on relay (`go-msgauth`)
- [ ] SPF/DKIM/DMARC verification
- [ ] Per-domain rate limiting
- [ ] DB-backed routing — would finally give the orphaned `Relays`/`RelayGroups`
      tables the webui writes to a consumer

## Known gaps and decisions

- **`smtpgreeting` is not reproducible.** go-smtp owns the banner string. Would
  need a small upstream patch adding a greeting hook.
- **Over-long DATA lines are counted, not rewritten** — deliberate; Haraka's
  `\r\n ` injection breaks DKIM.
- **No gateway column in the log tables.** Haraka and mailgw-go rows are only
  distinguishable by the `X-NGM-Gateway` header on relayed mail. A column would
  be better but needs a coordinated logservice change.
- **Plaintext relay credentials** still work for parity. `auth_pass_env` is
  supported and `check` warns; the committed mailtrap credentials under
  `deploy/mailgw/settings/mine/config/` should be rotated regardless.
