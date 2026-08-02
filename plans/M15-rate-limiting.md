# M15 — Rate limiting

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{ratelimit,smtpsrv,config,obs}`, `cmd/mailgw-go`  ·  **Depends on:** M11 (connection cap), M13 (for per-user limits)  ·  **Blocks:** —

> Source: `mailgw-go/TODO.md:159` ("Per-domain rate limiting").

## What is absent

No per-IP, per-sender, per-recipient-domain or global limiter exists anywhere.
`allowlistListener.Accept` (`internal/smtpsrv/listener.go:64-88`) is a boolean
allow/deny and nothing else, and `internal/obs/metrics.go:36-84` has no throttle
counters — which confirms the feature was never present rather than removed.

The asymmetry is the tell: **outbound is bounded** (`Concurrency`, `PerGroup` —
`internal/queue/runner.go:142,282-291`), inbound is not. An allowlisted peer that
misbehaves — a compromised application server, a loop, a credential-stuffing run
against M13's new AUTH — has no ceiling until it fills the spool.

M11.1 adds a **concurrency** cap (`max.connections`). This milestone adds
**rate** caps, which are a different thing: a peer opening one connection at a
time and sending a million messages through it never trips a concurrency limit.

## Scope

Four dimensions, in the order they earn their keep:

1. **Per-IP connection rate** — the cheapest, and the one that answers an
   abusive peer. Evaluated at `Accept`, beside the allowlist.
2. **Per-sender (MAIL FROM) message rate** — answers a runaway application.
3. **Per-authenticated-user rate and failed-AUTH rate** — needs M13. The
   failed-AUTH limiter is the one that makes AUTH safe to expose.
4. **Per-recipient-domain rate** — the item the TODO actually names. Note it is
   an *inbound* control on where mail is going, distinct from outbound
   `per_group_connections`, which limits connections to a relay.

Refusals are `421 4.7.0` (connection level) and `450 4.7.1` (transaction level) —
temporary, so a legitimate sender retries and a limiter misconfiguration does not
destroy mail. **Nothing here may return a 5xx**, for the same reason M7 bounces
only on 5xx: turning a tuning mistake into permanent rejection is the failure
mode to design against.

Count every refusal, per dimension. A limiter with no counter is a limiter
nobody can tune.

## State

In-memory sliding windows, per process.

`internal/store` exists (pure-Go SQLite, holding identity, settings and the
config cache) but is **the wrong home for a hot counter**: it is on the
management path, it is fsync-backed, and a rate limiter writing to it would put
disk I/O in the accept loop. The counters do not need to survive a restart —
losing them re-opens a window that was about to expire anyway.

That memory-only choice has a consequence worth stating: **the limits are
per-gateway, not per-fleet.** Ten edge nodes with a limit of 100/min admit
1000/min collectively. That is the right trade — a shared counter would mean a
network round trip in the accept path, and a management-plane failure would
become a mail failure, which is exactly the property `/readyz` was designed to
avoid (`internal/adminui/observe.go`).

Bound the maps. A limiter keyed on remote IP is itself a memory-exhaustion
vector if nothing evicts, and M11.4's MX cache is the cautionary example already
in the tree.

## Configuration

A `limits:` block in `server.yaml`, every key **omitted when empty** so an
unchanged bundle keeps hashing identically (`internal/config/bundle.go`), and
**every limit off by default**. This gateway's defaults do not silently start
refusing mail; `attach.enabled` and `outbound.reuse_connections` are both
shipped off for the same reason.

Decide whether limits hot-swap or need a restart, and put them on
`restartRequired` (`cmd/mailgw-go/gateway.go:696-729`) if they do not. Hot-swap
is worth the effort here — a limit an operator cannot adjust without restarting
a mail server during an incident is a limit they will not use.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
```

- Table-driven window tests with an injected clock. **`Date.now()`-style
  wall-clock reads in the limiter make the tests flaky** — take the clock as a
  dependency from the start.
- A listener test that exceeds the per-IP rate and asserts `421`, plus that a
  peer under the limit is unaffected.
- `go test -race` matters more than usual here: the limiter is touched from every
  accept goroutine.
- Assert the eviction path, not just the limit — a test that only checks
  refusals will pass with an unbounded map.

## Deliberately not done here

- **Greylisting.** It looks adjacent and is not: it needs *durable* triplet state
  (IP, sender, recipient) surviving restarts, which is the opposite of the
  memory-only decision above, and it delays legitimate mail by design. It is a
  separate milestone with a separate storage answer, and it is a poor fit for a
  gateway whose senders are mostly known.
- **Fleet-wide shared limits.** See "State" — a shared counter puts the
  management plane in the mail path.
- **DNSBL / RBL lookups.** Reputation, not rate. No consumer, and the allowlist
  makes it largely redundant for this deployment shape.
- **Outbound rate limiting per relay.** `outbound.concurrency` and
  `per_group_connections` already exist; a *rate* limit there is a separate
  question about what receiving relays will tolerate, and M7 already declined to
  guess at that when it shipped `reuse_connections` off.

---

## What was built differently

Four departures. Two are corrections to this file.

**1. The block is `ratelimit:`, not `limits:`.** `max:` already unmarshals into a
Go type called `config.Limits`, and `max.connections` (a concurrency cap)
sitting beside `limits.connect_per_ip` (a rate) is precisely the pair an
operator sets the wrong one of. The distinction the two blocks draw is *how many
at once* against *how often*, and the names now say so.

**2. Token buckets, not sliding windows.** The reason is the map bound this file
asks for. A sliding window keeps the timestamps of the events still inside it,
so its memory per key grows with the configured rate — a 10000/min limit keeps
up to 10000 timestamps for one peer, and "bound the maps" then means bounding a
quantity nobody can predict. A bucket is one float and one timestamp whatever
the rate.

It also makes eviction **provable rather than heuristic**, which is the part
worth keeping: a bucket refilled to capacity is byte-for-byte what the limiter
would create for that key, so dropping it cannot admit an event that would
otherwise have been refused. `evictLocked` is that sentence in code. The same
argument cannot be made about a window's timestamps, which have to be kept until
they expire. To an operator writing "100 per minute" the two behave identically.

**3. The per-IP limiter sits INSIDE the allowlist — the opposite side from
M11's connection cap.** This looks inconsistent and is not; the resource decides
the side, and the two bound different ones.

The cap bounds a process-wide semaphore, so a peer the allowlist is about to
refuse must not hold a slot — M11's "the cap would become the attack". This
limiter bounds nothing shared: every key has its own bucket, so an unlisted peer
cannot spend anybody else's allowance and there is no starvation to design
against. What it *does* cost is a map entry per distinct address, and outside
Guard that map is keyed by the internet rather than by the allowlist — which
would make the limiter itself the memory-exhaustion vector this file warns
about. It also sits *before* the cap, so a peer over its rate never spends a
semaphore slot on its way to being refused. Chain:
`tcp → proxyproto → Meter → tls → Guard → Throttle → Limit → srv.Serve`.

**4. Failed AUTH is answered `454`, not `421`.** This file says connection-level
refusals are 421, and 421 — "service closing transmission channel" — is the
honest code for what *should* happen to a stuffing run. But go-smtp's
`handleAuth` passes an `*SMTPError` straight to `writeError` (`conn.go:851-861`)
and never closes the connection, so answering 421 would announce a
disconnection that does not occur. `454 4.7.0` is RFC 4954's temporary
authentication failure, it is true, and it is a 4xx. What actually bounds the
connection is `inactivity_timeout` and `max.connections`.

### Three things the plan did not anticipate

**Only failures may spend the failed-AUTH budget.** The key is called
`auth_failures_per_ip`; checking and spending in one operation would make it
"AUTH commands per IP" and throttle exactly the clients that are behaving. That
is why `Limiter` has `Blocked` (query) and `Spend` (record) as well as `Allow`
(both) — the query deliberately does not allocate a bucket, or asking would be a
way to fill the map without ever sending anything.

**A full map admits rather than refuses.** When every tracked bucket is genuinely
in use there is nowhere to record a new key. Refusing what cannot be tracked
would turn a memory ceiling into a mail outage *and* hand an attacker a way to
deny service to everyone by filling the map with keys of their own choosing.

**The null sender is never limited.** Every bounce in the world shares it, so one
bucket for all of them would refuse exactly the notifications this gateway most
needs to deliver. More generally `Limiter.Allow` treats an empty id as unlimited:
an id the caller could not determine must not become the busiest key on the
gateway.

### Known behaviour worth stating

**Raising a limit gives faster relief, not instant relief.** The buckets survive
an apply — deliberately, so a routine deploy of something unrelated cannot
release a peer under pressure — which means raising 2/hour to 50/hour turns a
30-minute wait for the next event into a 72-second one rather than refilling
immediately. Crediting the difference would be the "released by a deploy"
behaviour the design exists to avoid, only triggered on purpose.
