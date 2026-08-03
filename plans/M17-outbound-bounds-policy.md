# M17 — Outbound bounds that need a policy first

**Status:** **done** (2026-08-03)  ·  **Packages:** `mailgw-go/internal/{deliver,config,obs,queue,node}`, `docs`  ·  **Depends on:** M11, M16  ·  **Blocks:** —

> Read [What was built differently, and why](#what-was-built-differently-and-why)
> first: both policy questions below were answered, and one of them was answered
> against this file's own suggestion.

> Source: the "Deliberately not done here" section of
> [M16](./M16-m11-reaudit-fixes.md). Both items were looked at during the
> resource-bounds work and deferred for the same reason, stated there: they are
> **questions rather than numbers**. M11 could add `max.connections` because the
> answer to "how many" is a number an operator picks. Neither of these has that
> shape.

## Why they are a milestone rather than two TODO lines

Every bound M11 and M16 shipped had an obvious refusal: over the cap, answer
421; over the size, answer 552; past the retention, delete. These two do not.

- Evicting a pooled connection means choosing **whose** connection to close, and
  the wrong choice makes delivery to a busy relay worse than not pooling at all.
- Caching a DNS failure means choosing **how long to keep believing it**, and
  the wrong choice leaves a domain unreachable after its DNS is fixed.

Getting either wrong is worse than the gap it closes. That is the whole reason
they are still open, and it is the thing to decide before writing code.

## M17.1 — A global cap on pooled connections

### What is absent

`Pool` bounds connections **per key** and nowhere else
(`internal/deliver/pool.go:28-42`):

```go
MaxPerRelay int   // idle connections held for ONE key
```

`poolKey` (`pool.go:67-72`) is built from `relay.Addr()`, `AuthUser`,
`TLSPolicy()` and `opts.LocalName` — so with `use_mx` on, the key set **follows
DNS**. `Expand` (`mx.go:232`) turns one `use_mx` relay into a synthetic relay per
MX host, and every distinct exchanger seen inside `connection_idle_timeout` is
its own key. The real ceiling is therefore:

```
MaxPerRelay × (distinct exchangers seen within connection_idle_timeout)
```

which no configuration key states and no operator can compute.

### Why it is a sharp edge and not a leak

M16 made `outbound.connection_idle_timeout: 0` a validation error and gave
`Pool.Reap` a schedule of its own, so idle connections are genuinely reclaimed
rather than held until the process exits. The set is bounded; it is just bounded
by something that is not written down.

It is also only reachable with **`outbound.reuse_connections` on, which ships
off** — so this is a ceiling on an opt-in path, not a default-path defect. That
is why it is here rather than in M11.

### The question to answer first

**What is evicted when the cap is reached, and who pays?**

Three candidate policies, and the trade is not obvious:

| Policy | Cost when wrong |
|---|---|
| Refuse to pool (dial fresh, do not cache) | Cheapest and never wrong — but the cap then silently disables pooling for the busiest relay, which is the one it was turned on for |
| Evict least-recently-used across all keys | A busy relay evicts a quiet one's warm connection, and the quiet one pays a full dial + TLS + AUTH on its next envelope |
| Evict from the largest bucket | Fairest, but a relay legitimately holding `MaxPerRelay` is penalised for being busy |

Note the first is not a joke: a pooled connection is documented as *"only ever an
optimisation"* (`pool.go:25-27`), and declining to pool is always safe. It may
well be the right answer, in which case M17.1 is ten lines and a counter.

### Shape once decided

- `outbound.max_pooled_connections`, **0 meaning the existing behaviour**
  (per-key only), so an upgrade changes nothing — the treatment
  `reuse_connections` and every `msgauth` key already get.
- Enforced in `Pool.Put`, which is the only place the set grows. Note M16 moved
  the `QUIT` out from under `p.mu`; whatever this does must not put a network
  round trip back under the mutex.
- A counter for the refusal. `internal/deliver` has **no `obs.Metrics` and no
  logger** — M10's decision, restated in M16's "Accepted as-is" — so the signal
  rides out on `Result` and the runner counts it, exactly as `Reused`,
  `TLSDowngraded` and `TLS` already do.

## M17.2 — Negative caching for MX failures

### What is absent

`Resolver.Hosts` (`internal/deliver/mx.go:85-135`) caches **successes only**.
Every failure path returns before `r.store(...)`:

```go
records, err := lookup(ctx, domain)
if err != nil {
    var dnsErr *net.DNSError
    if !asDNSNotFound(err, &dnsErr) {
        return nil, fmt.Errorf("mx %s: %w", domain, err)   // nothing cached
    }
    records = nil                                          // NXDOMAIN: implicit MX, cached as success
}
```

So while DNS is unavailable, **every envelope on every attempt** pays a fresh
lookup and its full timeout. With `outbound.concurrency: 10` and a queue
draining, that is ten simultaneous lookups against a resolver that is already
failing — the pattern that turns a slow resolver into an unavailable one.

Note the two failure kinds are already distinguished, and the distinction is
load-bearing here: `asDNSNotFound` (`mx.go:212`) separates "no such record",
which is the RFC 5321 §5.1 implicit-MX case and **is** cached as a success, from
a real resolution failure, which is not cached at all. A null MX
(`mx.go:120-124`) is a third case — a permanent, correct answer that is
currently not cached either.

### The question to answer first

**How long is a failure believed?**

The asymmetry is what makes this hard. A cached *success* that is stale costs
one delivery attempt to the wrong host, and the queue retries. A cached
*failure* that is stale means the gateway keeps refusing to try a domain whose
DNS came back — and `outbound.backoff` means the next attempt may be an hour
away, so a negative TTL of 5 minutes can cost far more than 5 minutes of mail.

Points to settle:

- **A separate, much shorter TTL than `DefaultTTL`.** Reusing the 5-minute
  success TTL is the obvious implementation and the wrong default: a failure is
  a statement about the resolver, not about the zone.
- **Whether a null MX is cached separately.** RFC 7505 is a permanent answer the
  domain published on purpose; it belongs with the successes, not here.
- **Whether the entry is dropped on the first success.** It should be, and that
  is nearly free — but it has to be stated, or a domain that recovers waits out
  the negative TTL for no reason.
- **Whether `SERVFAIL` and a timeout are the same thing.** A timeout may be this
  gateway's own network; a `SERVFAIL` is an answer.

### Shape once decided

- A `negative` flag plus an error on `cachedMX`, or a second map. One map keeps
  `store`'s existing bound (`maxCacheEntries`, `mx.go:58`) covering both, which
  matters — a second unbounded map would reintroduce exactly what M11.4 closed.
- The existing bound is a **constant, not a config key**, on the
  `internal/attach` `maxParts` precedent. A negative cache must not turn it into
  one.
- `Resolver` has no `Close()` and is rebuilt per bring-up
  (`mx.go:177-181`); do not give it a lifecycle to hold this.

## What must not regress

- **`outbound.reuse_connections` and the MX cache both stay off/unchanged by
  default.** Every knob here defaults to today's behaviour, so an upgrade that
  changes nothing in `server.yaml` changes nothing at runtime.
- **`internal/deliver` keeps no metrics and no logger.** Signals ride out on
  `Result` and the runner counts them.
- **No network round trip under `p.mu`** (M16.5), and **no allocation of a
  bucket to answer a query** — the shape M15's `Blocked` had to take for the
  same reason.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
```

All clean. **Every decision was confirmed against the unfixed code**, not merely
asserted: each new test was run with its own change reverted and observed to
fail, including each policy choice separately.

| Reverted | What failed |
|---|---|
| the global cap in `Put` | `Put did not report the global cap`; `3 idle connections, want 2`; `the refused connection was left open` |
| caching transient failures | `resolved 5 times, want 1 — the failure is not being cached` |
| the null-MX store | `resolved 2 times, want 1 — a null MX is not cached at all` |
| null MX at `negativeTTL` instead of the full TTL | `a null MX expired on the negative TTL` |
| failures sharing the success TTL | `a recovered domain was still refused from cache: mx partner.com: … server misbehaving` |
| the validation | `zero max pooled connections: expected a validation error` |
| the default | `reuse_connections alone should validate on the defaults` |
| the `counters` table row | all three obs guards, including `Metrics.DeliverPoolFull is in no counters entry` |

The fifth of those is worth keeping: reverting the TTL split reproduces, in a
test, the exact harm the 30-second number exists to prevent — a domain whose DNS
has come back still being refused out of cache.

New tests:

- `internal/deliver/pool_test.go` — `BoundsPooledConnectionsGlobally` (three
  relays under a cap of two: the third is refused, **closed**, and no incumbent
  is evicted), `ABusyKeyRecyclesItsOwnSlotAtTheCap` (the argument the whole
  policy rests on), `PerRelayRefusalIsNotReportedAsGlobal` (the counter must not
  fire on healthy pooling), `TheKeySetFollowsDNS` (three exchangers, three pool
  keys — the growth the cap exists for). Helpers `dialRelay` and `closed`: the
  cap tests drive `Put` directly, because what has to be asserted is the fate of
  the connection the pool turns away and `Deliver` never lets go of it.
- `internal/deliver/mx_test.go` — `CachesAResolutionFailureBriefly` (which also
  asserts `negativeTTL < 60s`, so the constant cannot drift past the shortest
  backoff without a test saying so), `AFailureIsNotHeldForTheSuccessTTL`,
  `ANullMXIsCachedAtTheFullTTL`, `ACachedFailureReadsIdenticallyToAFreshOne`,
  `TheNegativeCacheSharesTheBound`. All use the injected `Resolver.now`; nothing
  sleeps.
- `internal/config/config_test.go` — the two new invalid cases, **plus
  `connection_idle_timeout: 0`, which M16 added and never tested**, and a
  positive case asserting `reuse_connections: true` validates on the defaults
  alone, which is what makes enabling reuse a one-line change.

## What was built differently, and why

Both questions this file exists to ask were answered. The answers, and the one
place the answer went against this file's own suggestion.

**1. The pool refuses at the cap. It does not evict — and the cost this file
predicted for refusing does not materialise.**

The table above warned that refusing "silently disables pooling for the busiest
relay, which is the one it was turned on for". It does not, and the reason is
mechanical: `Get` **takes** a connection out of the pool, which frees its slot,
and `Put` returns it, which reclaims the same slot. A key already in the pool
recycles its own slot and never meets the cap. What the cap refuses is a **new**
key — an exchanger seen for the first time — which by definition has no warm
connection to lose. Refusal therefore protects incumbents and denies newcomers,
which is what eviction was supposed to buy.
`TestPool_ABusyKeyRecyclesItsOwnSlotAtTheCap` pins it.

That left the decisive argument as internal consistency. `mx.go:49-57` already
makes this exact call for the cache next door — *"an eviction policy that has to
be explained is worse than that"* — and `pool.go` documents a pooled connection
as *"only ever an optimisation"*. Both eviction policies make one relay pay for
another's traffic. So this file's own aside was right: M17.1 is a few lines and a
counter.

**A property nobody anticipated, and it is why `totalLocked` can be a scan.**
`setIdle` deletes a key the moment its slice empties, so every live key holds at
least one connection — which means a cap on connections is also a cap on **keys**.
That is precisely the unwritten ceiling M17.1 was written to close, closed by the
same line. It also makes summing the map on each `Put` cheap enough to prefer
over a maintained counter, which would have to stay correct across `take`, `Put`,
`expired`, `setIdle` and `Close`.

**2. The default is 256, not `0`-meaning-unchanged — against this file's own
"Shape once decided".**

That section proposed `0` meaning the existing behaviour, "the treatment
`reuse_connections` and every `msgauth` key already get". The wrong precedent was
cited. `max_pooled_connections` is not a *feature* toggle like `msgauth`; it is a
bound on a feature that is already opt-in, and its two immediate siblings —
`max_messages_per_connection` and `connection_idle_timeout` — are defaulted with
the comment saying why: *"Only consulted when reuse_connections is on, but
defaulted here so enabling it is a one-line change rather than three."* Zero would
have made it four.

So it follows M11's `max.connections` reasoning instead: defaulted, and **refused
when explicitly 0** while `reuse_connections` is on, with the same "set it high
rather than 0" wording. An explicit zero out of a console textarea would mean "no
global ceiling", which is the state the key exists to end. An upgrade still
changes nothing, because the whole pool is off by default.

**3. Thirty seconds, and the number is derived rather than chosen.**

The asymmetry this file identifies is real and it is what picks the number: a
negative entry must always be expired by the time an envelope retries, or the
cache becomes the reason a recovered domain stays unreachable. `outbound.backoff`
starts at 60s, so anything below that is safe and 30s leaves margin. The test
asserts `negativeTTL < 60s` so the constant cannot drift past the shortest
backoff silently.

What follows from that is the part worth recording: **if a negative entry never
survives to the next retry, it is not remembering a domain's health at all.** Its
entire job is the concurrent case — `outbound.concurrency` workers draining a
queue all resolving the same failing domain within the same few seconds. It is
stampede suppression with a short memory. That reframing is also the answer to
"should it be tunable": no, because nobody can measure a better value for a
window that exists only to deduplicate a burst.

**4. Three sub-decisions the shape section left open.**

- **A null MX is cached at the full TTL**, as this file suspected it should be.
  It was not cached at all before, so this is a small improvement in passing.
- **SERVFAIL and a timeout are not distinguished.** This file asked whether they
  should be, noting a timeout may be this gateway's own network. They are not: a
  timing-out resolver is *precisely* the case that produces the stampede, so
  excluding it would leave the main harm unaddressed while adding a branch.
- **"Dropped on the first success" needed no code.** A success simply overwrites
  the entry in the same map, which `TestHosts_AFailureIsNotHeldForTheSuccessTTL`
  pins.

**5. The cached error is returned verbatim, with no marker.**

The obvious touch is to say "(cached)" so an operator can tell. It was rejected:
this error becomes `Envelope.LastErr`, which M16.9 established is the *only*
evidence when no relay was contacted, and it is read back by both `mailq -json`
and the expiry DSN — which goes to a **sender** who has no idea this gateway has
a DNS cache. Two identical failures must not read as two different failures.

**6. No counter for M17.2, and the reason is that nothing became less visible.**

MX failure does not ride on `Result`; it surfaces in `queue/runner.go`, which
already logs `cannot resolve mail exchangers` **per envelope**. That line is
unaffected by the cache — only the DNS traffic collapses — so a DNS outage stays
exactly as visible as before. A counter would have meant plumbing a signal out of
`Resolver` through `targets()` to say something `deliver_deferred` and that Warn
already say. Same reasoning DMARC's absent counter got in M14. The snapshot
therefore goes 46 → **47**, not 48.

### Accepted as-is

- **`max_pooled_connections` is not read live**, unlike M15's rate limits.
  `outbound` is on the `restartRequired` list (a `reflect.DeepEqual` over the
  whole struct, so the new key needed no change there), and the rebuilt pool
  starts empty. M15 made its limits live because an operator retunes one
  mid-incident; this is a file-descriptor ceiling, and a restart is the honest
  cost while pooling is opt-in.
- **The per-key refusal is still uncounted.** It predates this milestone and is
  healthy behaviour — one relay at its configured depth. Counting it would make
  `deliver_pool_full` fire during normal operation and stop meaning "raise the
  number".

## Deliberately not done here

- **Making `maxCacheEntries` configurable.** It is a memory ceiling under a
  pathological input, not a throughput knob. Same standing as `internal/attach`'s
  `maxParts` and `maxDepth`.
- **A shared cache or pool across gateways.** Same answer M15 gave for shared
  rate-limit counters: it would put a network round trip in the delivery path and
  make a management-plane failure a mail failure.
- **Rate limiting per relay on the way out.** Named in M16's deferral list and
  still separate — `outbound.concurrency` and `per_group_connections` bound
  connections, and a *rate* there is a question about what receiving relays
  tolerate, which M7 already declined to guess at.
- **Per-IP and per-sender rate limiting**, the third item M16 deferred, is
  **done**: [M15](./M15-rate-limiting.md) shipped five dimensions rather than
  those two. It is named here only so the M16 list resolves completely.
