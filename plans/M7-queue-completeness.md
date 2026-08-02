# M7 — Queue completeness

**Status:** **done** (2026-07-30)  ·  **Packages:** `mailgw-go`, `logservice/migrations`, `webui-fastify`, `deploy`  ·  **Depends on:** M1  ·  **Blocks:** —

> Migrated verbatim from `mailgw-go/TODO.md` on 2026-07-29. This was the old
> "M3–M5" before central management renumbered the series.

## Goal

Finish the outbound queue. M1 built a spool that is durable and retries; what was
missing is everything an operator needs when delivery *doesn't* work — a way to
inspect and flush the queue, real bounces, a release path out of quarantine, and
the lifetime rules that stop mail sitting there forever.

Much of the original M3/M4 scope landed early with the runner. **M9.5 landed
here**, as its own plan said it should.

## Done

- [x] The scheduler sleeps until the next envelope is due (`Spool.ReadyAndNext`
      + `Runner.sleepFor`) instead of on a fixed tick, and a finished attempt
      nudges it. `poll_interval` is now a ceiling, so it can be raised without
      delaying retries. *(landed early, with M1)*
- [x] `mailq` / `flush` / `rm` / `release` / `hold` CLI over the spool
- [x] Shutdown waits for `Runner.Start` to return (M9.5)
- [x] Body reference counting without the O(queue) read scan in `gcBody`
- [x] A release path out of `quarantine/`
- [x] DSN / bounce generation (RFC 3464), delay warning, `max_lifetime` expiry
- [x] Never bounce a bounce
- [x] Real MX lookup (`use_mx`) for relays that want it
- [x] Connection reuse across envelopes to the same relay
- [x] **Inherited from M2:** a recipient refused by a *data-stage* rule now gets
      a DSN instead of a WARN

## Three defects found during design, fixed first

None were in M7's own list; all three sat underneath it, and building bounces on
top of them would have made each one worse.

1. **`gcBody` deleted bodies still in use.** It scanned only `q/` and
   `inflight/`, but `split()` buckets by `(relay group, quarantined, headers)` —
   so one transaction can produce a queued envelope *and* a quarantined one
   sharing a body. Completing the queued half deleted it. Invisible until now
   only because nothing ever released from quarantine, which is exactly what
   this milestone adds: `release` would have handed a worker an envelope
   pointing at nothing.
2. **Two paths never checked `max_lifetime`.** `Runner.attempt` returned early
   for an unknown relay group and for an unopenable body, both *before* the
   lifetime check at the tail. An envelope whose relay group was renamed out of
   the configuration retried forever — never expired, never reached `dead/`, and
   after this milestone would never have bounced either. Expiry now lives in
   `deferEnvelope`, which every non-terminal path passes through.
3. **The backoff never grew on those same two paths**, because `env.Attempts++`
   also sat past their early returns. They retried every 60 seconds indefinitely.

## What was built differently from the plan

**The counter file was rejected.** The original bullet asked for reference
counting via a counter file rather than the directory scan. Incrementing and
decrementing a counter is not crash-atomic: a lost decrement leaks a body, but a
lost increment **deletes a live one**. Instead `gcBody` resolves referrers from
*filenames* — a body is `data/<txn>.eml` and an envelope may only reference its
own transaction's body, so almost every envelope is ruled out without being
opened. That invariant is now enforced in `Envelope.validate` rather than merely
observed, because the shortcut is unsound without it in the direction that loses
mail. Same O(dirents), no file reads, and no new state that can drift.

**Bounces are routed by the rule engine, with a named fallback.** A bounce goes
to the original *sender*, and a rule set written around recipient domains often
has nothing that matches one. The tempting fallback — the relay group the failed
message was using — is a guess: that group is the smarthost for the *recipient's*
domain and would usually answer "relay denied", so the bounce would die in
`dead/` and the sender would learn nothing. `dsn.relay_group` is the explicit
fallback, validated at `check` time against the relay table.

**`use_mx` is "smarthost named by domain", not direct-to-MX.** The relay's
`exchange` becomes a domain whose MX records are resolved at delivery time. It
does **not** consult the recipient's own domain: an envelope groups recipients by
relay *group*, so per-recipient-domain delivery would additionally need `split()`
to bucket by domain, and carries its own TLS and reputation story. That is a
different delivery mode, and out of scope here.

**Connection reuse ships off by default** (`outbound.reuse_connections`). Turning
it on changes what every relay in the field sees from this gateway — many cap
messages per connection, many rate-limit per connection rather than per message —
and none of that is observable from the gateway. It is tested and one key away,
the same treatment `attach.enabled` gets.

**M9.5's first bullet was already fixed.** `gateway.shutdown()` already selected
on drain-versus-deadline; the plan pointed at `main.go`, where the code no longer
lived. The remaining three bullets landed.

## The DSN design

`internal/dsn` builds the message and knows nothing else — no spool, no routing,
no configuration — so it is pinned by a golden file and reused by both callers.
`queue.Bouncer` injects the result, and is a standalone type rather than a
`Runner` method because the SMTP session is its second caller.

**Identity nests rather than minting a fresh root.** A bounce for delivery
`X.1.2` is `X.1.2.<n>`. `uuidx.ID.Valid` allows arbitrary depth and `validate`
only checks the literal prefix chain, so `WHERE uuid LIKE 'X%'` still finds the
whole tree — the contract `internal/uuidx` and `tests/smtp` both rest on — and
the id itself says which delivery bounced. A fresh root would have produced a
`Delivery` row with no `Connection` and no `Transaction` anywhere. `<n>` comes
from `Envelope.DSNSeq`, because a delay warning and a later failure would
otherwise share a uuid and therefore a queue filename, and the second would
silently overwrite the first.

**Four triggers, one report each.** Permanent rejection and expiry are collected
once at the end of an attempt, so an envelope with several rejected recipients
produces one report naming all of them — which is what the per-recipient section
of RFC 3464 is for. The delay warning lives in `deferEnvelope`. The fourth is the
SMTP session, for recipients refused after the message was already accepted.

**Only 5xx becomes a bounce.** A recipient can also fail here on a 4xx —
most often `ruleset.DefaultAction()`, the 451 that preserves Haraka's DENYSOFT at
`npRoute.js:65`. Bouncing that would turn a gap in the routing configuration into
permanent rejections for mail that would deliver fine once the gap was noticed.

**Enqueue before `Complete`/`Bury`, deliberately.** A crash in between re-queues
the pre-attempt envelope and bounces twice; the other order loses the bounce
outright. That is the spool's standing trade — a duplicate is recoverable,
missing mail is not — and it means `DSNSent` is not an exactly-once guarantee.
Its job is stopping a repeat on the *next* attempt, which would otherwise recur
every retry for four days.

**`dead/` is metadata-only.** `Bury` collects the body, so a buried envelope
records what happened and to whom and cannot be resurrected. The alternative —
`dead/` pinning bodies — means a queue that has been given up on still consumes
the disk of everything in it, with nothing to drain it. Hence no `mailq requeue`.

## Repo defect found while finishing

**The root `.gitignore` excluded `mailgw-go/internal/queue/` entirely.** A bare
`queue` pattern matches a directory of that name at any depth, so the whole
outbound spool package had never been committed — along with the sample payloads
under `{legacy/,}deploy/core/samples/queue`. Anchored to `/legacy/mailgw/queue/`,
which is the only spool that actually lives inside the repo.

## Related defects, closed

**[M9.3](./M9-correctness-and-durability-fixes.md)** was already done.
**[M9.5](./M9-correctness-and-durability-fixes.md)** closed here: the teardown is
now an explicit sequence (listeners → SMTP drain → cancel the runner → wait for
it → close the event pipeline) under one `server.shutdown_timeout` budget, and
`events.Client.Close` takes a context so in-flight retries abort and the
remainder spills to `failed-events/`. Measured: 53 ms on an idle gateway, against
a 10 s grace period.
