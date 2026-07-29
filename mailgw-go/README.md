# mailgw-go

A Go rewrite of the Haraka SMTP gateway in `mailgw/`, built on
[`emersion/go-smtp`](https://github.com/emersion/go-smtp).

It runs **side by side** with Haraka on a separate port (2525 by default) while
parity is proven. Haraka keeps port 25; nothing about the existing service
changes.

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

./mailgw-go check -config ./testdata/config    # validate config; non-zero on error
./mailgw-go serve -config ./testdata/config    # run
./mailgw-go fields                             # list every matchable field
./mailgw-go convert-routing routing.json > routing.yaml
./mailgw-go explain -config ./testdata/config \
    --ip 10.20.0.5 --helo box --from a@x.com --rcpt b@ngm.dev [--eml msg.eml]
./mailgw-go -version
```

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

Lives in one directory (`/opt/mailgw-go/config` in the container). Three files
are reused from Haraka **unchanged** — `relays.json`, `ngmfilter.json`,
`logging.json` — and `routing.json` is read in its existing format.

`server.yaml` replaces `connection.ini`, `smtp.ini`, `log.ini`, `me`,
`smtpgreeting` and `host_list`. See `testdata/config/server.yaml`, which is
annotated with the Haraka setting each value came from.

`routing.yaml` is the rule file described above. It is optional: without it,
`routing.json` is read in its existing Haraka format and transpiled.
`testdata/config/` ships a worked `routing.yaml`; `config/` deliberately keeps
only `routing.json`, so CI exercises both paths.

## Testing

`internal/smtpsrv/contract_test.go` ports every assertion from
`tests/smtp/tests/smtp.test.ts` so the SMTP contract runs under `go test`,
without Docker. The Bun suite itself runs unmodified against the real binary:

```bash
SMTP_PORT=2525 bun test tests/smtp                    # from the repo root
MAILGW_DB_CHECK=1 SMTP_PORT=2525 bun test tests/smtp  # also checks the DB rows
```

## Things to know

- **The IP allowlist is the only gate by default.** With no policy rule saying
  otherwise the recipient stage accepts every address, matching `npFilter.js:73`.
  Starting with an empty allowlist is refused unless `allow_all: true` is set
  explicitly.
- **A recipient refused only at DATA is dropped, not bounced.** That happens
  when a data-stage rule refuses one recipient of several. There is no reply
  left to put it in, and DSN generation is M7; until then it is logged at WARN.
  Rules that can decide at RCPT do not have this problem.
- **A crash mid-SMTP can redeliver.** Inherent to any spooling MTA: the
  alternative is losing mail.
- **`outbound.poll_interval` is a ceiling, not a tick.** Queue filenames carry
  their due-second, so the scheduler sleeps until the next envelope is actually
  owed; a new message wakes it immediately, and a finished attempt wakes it to
  re-evaluate. The interval only bounds how long something placed in `q/` by
  hand — releasing a quarantined message, say — can sit unnoticed, so it is
  cheap to raise. Due times are truncated to the second, so work can start up
  to a second early and never late.
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
