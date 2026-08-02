# M17 — Outbound bounds that need a policy first

**Status:** planned  ·  **Packages:** `mailgw-go/internal/{deliver,config,obs}`  ·  **Depends on:** M11, M16  ·  **Blocks:** —

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

- **A decision recorded before code.** Both items above are one policy question
  each; write the answer into this file's "What was built differently" section
  with the reasoning, because the number will look arbitrary to whoever reads it
  next.
- `internal/deliver` tests already stand up **real go-smtp instances as fake
  relays** and inject `Lookup`; neither item needs a new harness.
- **Assert the eviction, not just the cap** — M15's plan makes the same point,
  and for the same reason: a test that only checks refusals passes against an
  implementation that never evicts.
- **A clock is injected already** (`Resolver.now`, `mx.go:34`). Use it; a
  negative-TTL test that sleeps is a flaky test.
- For M17.1, a test with `use_mx` and several exchangers, since that is the only
  way the key set grows and therefore the only way the global cap is reachable.

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
