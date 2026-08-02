# The mail path

Everything from `accept(2)` to a delivered message, and where each piece lives.

## Inbound

```
tcp → proxyproto → Meter → tls.NewListener → Guard → Limit → srv.Serve
```

That chain is not arbitrary and each link has a reason:

**`proxyproto`** (`internal/proxyproto`) is innermost because the PROXY header
precedes the TLS handshake, and `tls.Conn.RemoteAddr` delegates downward.
Hand-rolled v1 and v2, stdlib only.

The header **cannot** be parsed lazily on first `Read`, nor inside
`RemoteAddr()` — the allowlist reads `RemoteAddr()` synchronously in its own
accept loop, so either approach would decide allow/deny before the header was
known, or block that loop. Instead the trust check runs before the first byte,
and a bounded worker pool resolves headers off the accept path.

**`Meter`** sits *below* TLS and hands out an unarmed connection-cap slot.

**`Guard`** (`internal/smtpsrv/listener.go`) is the IP allowlist. It has to be in
front of the server because go-smtp calls `NewSession` at `EHLO` — denying
*before* the banner has to happen in the listener.

**`Limit`** is the connection cap's admission decision, and it sits **outside
`Guard` deliberately**. Inside it, a peer the allowlist is about to refuse would
hold a slot until `Guard` closed it, so a flood from unlisted addresses would
fill the semaphore and throttle real senders — the cap would become the attack.

::: danger Do not wrap the accepted connection
`Limit` arms the slot it finds beneath the connection via `NetConn()` and hands
the connection on **unwrapped**. go-smtp finds TLS with a bare
`c.conn.(*tls.Conn)` assertion and offers no unwrap interface — a wrapper hides
it, which costs an `implicit_tls` listener its TLS identity in the rules, in the
`Received:` header and in the audit row, *and* drops the only pre-handshake read
deadline the server arms. A silent peer then holds its slot for ever.

This was a real defect, introduced by the milestone that added the cap and found
by the re-audit of it. See `plans/M16-m11-reaudit-fixes.md`.
:::

## Session

`internal/smtpsrv/session.go` — one `session` per connection, holding the
connection-scoped facts, the transaction in progress, and the ruleset snapshot.

**The ruleset is snapshotted once, at connect.** A reload mid-session must never
change the policy a message is being evaluated against, or a message could be
accepted under one configuration and routed under another.

**The identity lives on the session, not on the rule environment.** `Mail()`
rebuilds the helo scope from `heloEnv()` on every transaction — because STARTTLS
may have upgraded the connection since the session was created — so anything
written directly into the environment is silently discarded at `MAIL FROM`.

**`refuse()` is the only place message-scoped refusal counters move.** Three
sites turn a policy action into a transaction's final answer; funnelling them
through one function is what stops them drifting apart or double-counting.

## Rules

`internal/ruleset` — the reason the rewrite exists.

Predicates are a typed AST: `all`/`any`/`not`/`every`/`always` over
`field`/`op`/`value` leaves. **Every field name is checked against a registry**
(`schema.go`) carrying its stage and type, so a typo or a type mismatch is a
load-time error rather than a rule that silently never fires.

Stage inference is what fixes recipient-stage timing: a rule's stage is the
latest stage of any field it mentions, and `Route` walks rules in priority order
and stops at the first one needing a later stage. That is what makes an early
decision provably equal to the one `DATA` would reach — and therefore safe to
cache.

Two glob dialects, deliberately: `*` stops at a dot for domain-shaped fields and
crosses dots elsewhere. Implemented in `glob.go` rather than taking a dependency.

## Spool

`internal/queue` — `tmp/ data/ q/ inflight/ dead/ quarantine/ failed-events/`.

Every transition is a `rename(2)` within one filesystem. Queue filenames carry a
12-digit zero-padded due-second, so a lexical sort is due order and nothing has
to be opened to find the next job — which is also what lets the scheduler sleep
until something is actually due.

**Body ownership is resolved from filenames**, not by parsing every queued
envelope. A body is named for its owner, and an envelope may reference one named
for itself or for an ancestor. A reference-counting file was considered and
rejected: increments and decrements are not crash-atomic, and while a lost
decrement leaks a file, **a lost increment deletes a live body**.

`Envelope.validate` and `Spool.bodyReferenced` share one `bodyOwnedBy` helper for
that rule, because the second uses it as a shortcut to avoid opening every
candidate — and if the two disagreed, the disagreement would delete live mail.

::: warning New envelope fields must be `omitempty` at version 1
Bumping `EnvelopeVersion` makes `validate` reject every envelope already on disk,
and `Claim` answers that by moving them to `dead/` — an upgrade would park the
entire live queue.
:::

## Delivery

`internal/deliver` — one attempt per envelope, walking the relay group in
priority order.

`Deliver`'s defers run **LIFO**, which becomes load-bearing once connection
pooling is on: the pool disposition runs before the guard release, so
`guard.release()` is the first statement inside that defer.

Outbound TLS splits by policy. `opportunistic` gets `InsecureSkipVerify`
deliberately — RFC 7435 opportunistic security is encryption *without*
authentication, and verifying under it bought nothing because the only thing on
the other side of the failure was a cleartext redial. `required` verifies fully.

## Audit events

`internal/events` — bounded channel, sender pool, timeout and retry, spilling to
`failed-events/` on a `4xx` or after exhausting retries. `Send` never blocks; a
full buffer drops with a counter.

Events are **at-least-once and possibly delayed**. The mail path never waits on
them.

Nothing closes the event channel, ever. `Send` after `Close` returning silently
was one bug; `close(c.ch)` making a racing send **panic the process from the
audit path** was the worse one.

## Shutdown

An ordered teardown under one `server.shutdown_timeout`:

1. stop listening
2. drain SMTP sessions
3. cancel the delivery runner — **and wait for it**
4. stop the failed-events replayer (it and the next step both write
   `failed-events/`)
5. close the event pipeline

The shutdown context is re-read per attempt, so a sender already inside a retry
schedule cannot hold `context.Background()` for its whole backoff while shutdown
waits. Measured at 53 ms on an idle gateway, where it used to be a flat 10 s.

The container's `stop_grace_period` must exceed the timeout or the runtime kills
the process part-way through and the ordering buys nothing.
