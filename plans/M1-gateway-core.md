# M1 — Gateway core: SMTP, spool, delivery, events

**Status:** done  ·  **Package:** `mailgw-go`  ·  **Depends on:** —  ·  **Blocks:** M2, M7, M8

> **Reconstructed, not original.** M1 was never written up as a plan — it
> predates this directory, and `mailgw-go/TODO.md` only ever recorded it as the
> half-sentence "M1 and M2 are done". This file documents what M1 actually
> delivered, derived from the shipped code and the `mailgw-go` section of
> `CLAUDE.md`. It is here so the milestone series has no gap; treat it as a
> record of the finished state rather than as the plan that produced it.

## Goal

Replace the Haraka plugin pipeline with a single Go binary that accepts inbound
SMTP from allowlisted peers, routes each message to a relay group, spools it
durably, delivers it with retry and failover, and posts audit events to
logservice — without changing the config files or the database contract.

Two limits of the Haraka plugin set are what justified a rewrite rather than
more plugins:

- **Routing was four fields and one operator.** `Route.js:44` did
  case-insensitive equality, ANDed, so `ngm.dev` did not match `mail.ngm.dev`.
- **Delivery events were being lost.** `npLogDelivery.js:37` sent
  `rcpt_accepted` as a comma-joined list where logservice validates a single
  address, so every multi-recipient delivery 400'd — invisibly, because the POST
  is fire-and-forget with no `response.ok` check (`functions.js:91`).

M1 built the machine; **M2** built the rule engine that fixed the first defect.

## What shipped

- [x] **Inbound SMTP** on `emersion/go-smtp` v0.24.0 — `internal/smtpsrv`.
- [x] **Allowlist as a `net.Listener` wrapper** (`internal/smtpsrv/listener.go`).
      go-smtp calls `NewSession` at EHLO, which is too late to reproduce
      Haraka's `hook_connect` DENYDISCONNECT (`npFilter.js:69`), so the check
      sits in front of the server and the listener speaks the rejection itself.
- [x] **Fail-closed allowlist** (`internal/config/allowlist.go`), now CIDR- and
      IPv6-aware. A missing or malformed `ngmfilter.json` denies everything, and
      the zero value denies, so ignoring a load error cannot open the relay.
      Starting with an empty allowlist is refused unless `allow_all: true`.
- [x] **Config compatibility.** `relays.json`, `ngmfilter.json`, `logging.json`
      and `routing.json` are reused **unchanged**; `server.yaml` replaces
      `connection.ini` / `smtp.ini` / `log.ini` / `me` / `smtpgreeting` /
      `host_list`.
- [x] **UUID hierarchy** (`internal/uuidx`): `Connection.uuid` = `X`,
      `Transaction.uuid` = `X.1`, `Delivery.uuid` = `X.1.1`. A hard contract —
      `tests/smtp/tests/smtp.e2e.test.ts` finds all three with
      `WHERE uuid LIKE 'X%'`.
- [x] **Per-recipient split** (`session.split`): recipients are grouped by relay
      group and each group becomes its own envelope `X.1.<k>`, sharing one
      spooled body. Haraka could not do this at all — `npRoute.js:55` routed the
      whole message by `rcpt_to[0]`.
- [x] **Durable spool** (`internal/queue`): `tmp/ data/ q/ inflight/ dead/
      quarantine/ failed-events/`. Queue filenames carry a 12-digit zero-padded
      due-second, so a lexical sort is due order and nothing needs opening to
      find the next job. Recovery moves `inflight/` back to `q/`.
- [x] **Delivery with failover** (`internal/deliver`): per-recipient outcomes so
      one bad address cannot sink the others, relay-group failover on
      connection-level errors, TLS policy per relay, AUTH refused over an
      unencrypted link unless `allow_insecure_auth`.
- [x] **Audit events** (`internal/events`): bounded channel → sender pool →
      timeout + retry, spilling to `queue/failed-events/` on a 4xx (schema
      mismatch, not worth retrying) or after exhausting retries. `Send` never
      blocks; a full buffer drops with a counter. **One `/api/delivery` POST per
      recipient**, which is what fixes the lost-delivery defect above.
- [x] **The `250` reply contract.** `Session.Data` returns
      `*smtp.SMTPError{Code: 250, ...}`: go-smtp's `dataErrorToStatus`
      (`conn.go:1257`) passes an SMTPError's code through verbatim, and that is
      the only way to control the success text. It must contain "queued" and
      embed the txn id in parens — `tests/smtp/src/smtp.ts:143` scrapes it with
      `/\(([0-9A-F-]+)(?:\.\d+)?\)/i`.
- [x] **Contract tests.** `internal/smtpsrv/contract_test.go` ports every
      assertion from `tests/smtp/tests/smtp.test.ts` so the SMTP contract runs
      under `go test` without Docker. The Bun suite runs **unmodified** against
      the binary via `SMTP_PORT=2525 bun test tests/smtp`.

### Hook mapping

| Haraka | mailgw-go |
|---|---|
| `hook_connect` deny (`npFilter.js:49`) | `net.Listener` wrapper (`internal/smtpsrv/listener.go`) |
| `hook_rcpt` (`npFilter.js:73`, accepts all) | `Session.Rcpt` — policy, then a tentative `Route` |
| `hook_data` (`npData.js:9`) | start of `Session.Data` — posts `/api/connection`, same timing |
| `hook_queue_outbound` (`npQueue.js:9`) | `queue.Enqueue` — posts `/api/queue`, writes envelopes |
| `hook_get_mx` (`npRoute.js:48`) | `ruleset.Route` + `relays.Table.Lookup`, **per recipient** |
| `hook_delivered` (`npLogDelivery.js:9`) | queue worker → **one `/api/delivery` per recipient** |

## Companion change in `logservice`

The delivery schema was incompatible with an outbound queue, not merely strict:
`sender` rejected the null sender every bounce uses, `host` rejected
`localhost` / `dev-mailhog` / IP literals, and `ip` was IPv4-only.
`src/validation/delivery.ts` now accepts these; `migrations/015` widens
`Delivery.response` / `rcpt_list` to TEXT and adds the first indexes on `uuid`
columns. `rcpt_list` / `rcpt_accepted` stay **single-valued** — that is the
contract mailgw-go honours.

## Known consequences

- **A crash mid-SMTP can redeliver.** Inherent to any spooling MTA; the
  alternative is losing mail. See **M9.3** for a *second* duplication path that
  is **not** inherent and should be fixed.
- **Events are at-least-once and possibly delayed** — a real change from
  Haraka's fire-and-forget. Mail flow never waits on the audit trail.
- **`smtpgreeting` is not reproducible.** go-smtp owns the banner string; it
  would need a small upstream patch adding a greeting hook.
- **Over-long DATA lines are counted, not rewritten** — deliberate, because
  Haraka's `\r\n ` injection breaks DKIM.
