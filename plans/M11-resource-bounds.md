# M11 — Resource bounds

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{smtpsrv,config,central,deliver,queue,events,obs}`  ·  **Depends on:** —  ·  **Blocks:** —

> Source: the audit of 2026-08-01. Nothing here changes mail semantics. Every
> item is a ceiling that does not currently exist, or an error that is currently
> swallowed on a path where it is the only evidence.
>
> Can land alongside M10 or M12; nothing in it is sequenced.

The gateway is careful about the resources it was designed around — the spool is
claimed by rename, bodies are garbage-collected, outbound has `concurrency` and
`per_group_connections`, MIME depth and part count are hard constants. The gaps
are on the inbound side and in the long-lived caches, where nothing bounds
growth at all.

## M11.1 — no inbound connection cap

`Limits` (`internal/config/config.go:89-95`) has `bytes`, `line_length`,
`recipients`, `received_headers`, `header_lines` — no `max_connections`. go-smtp
spawns one goroutine per accepted connection with no cap of its own, and
`smtpListeners.start` (`cmd/mailgw-go/listeners.go:78-83`) hands the listener
straight to `srv.Serve`. Each connection carries a `bodyScan` and, once DATA
starts, a spool temp file, so the cost is file descriptors and disk, not just
memory. `internal/obs/metrics.go:36-84` has no throttle counter, confirming this
was never present rather than removed.

Outbound is bounded (`RunnerConfig.Concurrency`, `PerGroup` —
`internal/queue/runner.go:142,282-291`). Inbound is not.

**Fix.** `max.connections`, enforced by a semaphore in a listener wrapper beside
`Guard`. Over the limit, answer `421 4.7.0 Too many connections` and close —
`421` is the code that tells a sending MTA to come back rather than to give up.
Count refusals. Default it high enough that nobody meets it by accident, and
document the interaction with `inactivity_timeout`: a cap without a timeout is a
denial-of-service primitive rather than a defence, which is why M10.3 validates
that timeout.

## M11.2 — the config bundle decode is unbounded

`internal/central/client.go:338`:

```go
if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
```

No `io.LimitReader`, unlike the error path immediately below it at `:349`, which
caps at 2 KiB (`reasonFrom`). A broken or hostile console — and `central_insecure_tls`
(`internal/store/settings.go:17-21`) means the channel is not always
authenticated — can drive the gateway out of memory. Cap it at a size a real
bundle cannot approach and fail with a named error.

## M11.3 — `failed-events/rejected/` has no drain and no gauge

Already recorded at `mailgw-go/TODO.md:125-129`. Like `dead/`, it accumulates
and only `mailgw-go events rm` empties it. That the files are kept is deliberate
— they are the evidence of what logservice refused — but **nothing warns when it
grows**, and `mailgw_failed_events` counts only the *pending* directory
(`internal/obs/metrics.go` gauges, fed by `Spool.LenAll()`).

**Fix.** Add the `rejected/` count as its own gauge, and a retention sweep with
a configurable age. Follow the precedent `LenAll` already sets for
`failed-events/`: tolerate the directory being absent, because a spool from
before it existed is a valid spool and "no such directory" means zero, not
broken.

## M11.4 — the MX cache is never evicted

`deliver.Resolver.cache` (`internal/deliver/mx.go:37-38`, written at
`:151-158`) is a `map[string]cachedMX` whose TTL is checked **on read only**.
Nothing ever deletes an entry, so with `use_mx` against many domains it grows for
the process lifetime. Add eviction — a sweep on write, or a bounded map.

## M11.5 — the connection guard does not cover pooled connections

`newConnGuard` is armed at `internal/deliver/client.go:137`, but `guard.use(conn)`
is only ever called inside `connect()` (`:306`, `:337`). A connection returned by
`acquire()` (`:142`, `:257-277`) never reaches it, because `Pool.Get` returns a
bare `*smtp.Client` with no `net.Conn` attached.

So with `outbound.reuse_connections` on, **context cancellation cannot interrupt
an in-flight DATA on a reused connection** — it blocks until `SubmissionTimeout`
(defaulting to `data_timeout`, 10 minutes), well past `server.shutdown_timeout`
and past everything M9.5's ordered teardown was built to bound.

`pooled.conn` and `type closer interface{ Close() error }`
(`internal/deliver/pool.go:45,50`) are **declared and never assigned or read** —
the half-built other end of exactly this. Either finish it (carry the `net.Conn`
through the pool so `guard.use` can reach it) or delete the dead fields; do not
leave a field that looks like the fix.

Mitigating, and the reason this is M11 rather than M10: `reuse_connections`
ships **off** (`mailgw-go/TODO.md:56-59`), so nothing in the field is affected
today. It is a trap for whoever turns it on.

`Pool.IdleTimeout` is enforced lazily inside `Get` only (`pool.go:85-88`), so a
relay that stops receiving traffic keeps its sockets and FDs until `Close()` at
process shutdown (`:145-159`). Fix in the same pass or record it here as
accepted. **Fixed in the same pass**: `Pool.Reap`, started from `bringUp` only
when `reuse_connections` is on, collecting under the lock and closing outside it
the way `Close` already does. It also drops map keys whose slice empties — `take`
left a zero-length slice under every relay the gateway had ever spoken to, and
`poolKey` is built from `relay.Addr()`, so with `use_mx` that key set follows DNS
rather than the configuration.

## M11.6 — MX resolution failure loses its error

`Runner.targets` (`internal/queue/runner.go:270-275`) logs a `Warn` and
`continue`s. If every member of a relay group is `use_mx` and DNS is down,
`targets` returns an empty slice: the relay loop at `:339` never executes, `last`
stays nil, `env.Done()` is false, and the envelope is deferred with
**`env.LastErr` unchanged from the previous attempt**.

So the operator sees a stale reason in `mailq`, and the eventual DSN carries a
diagnostic that has nothing to do with why delivery failed. Set `LastErr` from
the resolution failure when no target could be built.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
```

- **11.1** needs a listener test that opens `max.connections + 1` sockets and
  asserts the last gets `421` and is closed.
- **11.5**, if finished rather than deleted, needs a `pool_test.go` case that
  cancels the context mid-DATA on a reused connection and asserts the call
  returns promptly rather than on `SubmissionTimeout`.
- **11.6** needs a `runner` case with a `use_mx` group and a failing resolver,
  asserting the deferred envelope's `LastErr` names DNS.
- Any new counter or gauge fails `internal/obs/metrics_test.go` by design — the
  reflection test requires every `atomic.Int64` in the name table exactly once,
  and the golden snapshot-key count must be bumped deliberately.
- Any new config key needs a bundle round-trip test
  (`cmd/mailgw-go/bundle_test.go`) and, if it cannot hot-swap, an entry in
  `restartRequired` (`cmd/mailgw-go/gateway.go:696-729`).

**How it was actually verified.** `gofmt -l . && go vet ./... && go test -race
./...` clean, and `check` against both `./config` and `./testdata/config`.
`restartRequired` needed **no edit**: it compares `Server.Max` and
`Server.Events` with `!=`, and both new keys are comparable scalars inside those
structs. `TestLoadBundle_EqualsFileMode` is the bundle round-trip test the
milestone asked for — it parses `testdata/config/server.yaml` through the strict
(bundle) *and* lax (file) paths, so adding the keys there exercises both.

M11.1 was also driven end to end against a running binary, since it is the only
item with an externally visible protocol effect: with `connections: 2`, the third
concurrent socket read `421 4.7.0 Too many connections, try again later` and was
closed, one WARN was logged, and a fourth connection was greeted normally once a
held session ended.

## M11.7 — `events.Stats.Dropped` was never exposed

Folded in from `mailgw-go/TODO.md`, which asked for it: buffer-full drops were
counted per client (`internal/events/client.go:24`) with no `obs.Metrics` field,
so the one **unrecoverable** form of audit loss was also the only one invisible
to `/metrics` and the console heartbeat — `EventsSpilled`, `EventsReplayed` and
`EventsReplayFailed` were all exposed, and every one of those still has the row
on disk.

`events_dropped` / `mailgw_events_dropped_total`, incremented beside
`Stats.Dropped`. Its HELP string is explicit that a dropped event is gone where a
spilled one is parked, because a dashboard that treats the two as the same
number is worse than one that shows neither.

## What was built differently

Five things the plan above did not anticipate. Each was found by writing the
code or the test.

**1. The connection cap goes OUTSIDE `Guard`, not "beside" it.** The plan said
"a semaphore in a listener wrapper beside `Guard`" without saying which side, and
the two are not equivalent. Inside — `tls → limit → Guard` — a peer the allowlist
is about to refuse holds a slot for the moment before `Guard.deny` closes it, so
a flood from unlisted addresses fills the semaphore and gets legitimate senders
`421`'d: **the cap becomes the attack**. Outside, a slot is only ever spent on a
peer that already passed. The chain is now:

```
tcp -> proxyproto -> tls.NewListener -> Guard -> limit -> srv.Serve
```

The consequence is that `conn_throttled` is a **subset** of `conn_accepted`, not
a sibling — stated in its HELP string, the way `proxy_dropped` states the
opposite. One semaphore is shared by every listener, because `max.connections`
sits under `max:`, which is server-wide, and because the resource it bounds is
this process's file descriptors rather than any one socket.

`limitConn.Close` needs a `sync.Once`: go-smtp closes the session's connection
and `Server.Close` closes every live one again at shutdown, so an unguarded
release would let the cap drift upward until it bounded nothing.
`TestLimit_DoubleCloseReleasesOneSlotOnly` is that guard.

**2. Bounding the bundle decode is not enough — the deferred drain reads the
same body.** `central.do` has a `defer io.Copy(io.Discard, resp.Body)` for
connection reuse, and it runs on the failure path too, so capping only the
decoder would have fixed memory and not bytes-read. The limit is applied **once**
to a `body` reader that the drain, `reasonFrom` and the decode all share.
Separately, `json.NewDecoder(...).Decode` had to become read-then-`Unmarshal`:
streaming into a decoder cannot distinguish "the body was too big" from "the JSON
was truncated", and a gateway that cannot say which sends an operator after the
wrong problem.

`ErrResponseTooLarge` is deliberately **not** an `*HTTPError`, so `IsUnreachable`
reports true and the poll loop treats it exactly as it treats an outage: keep the
last-good configuration, retry next tick.

**3. `Deliver`'s defers run LIFO, so `Pool.Put` runs before `guard.release()`.**
This is invisible until M11.5 is fixed, and then it is load-bearing: once
`guard.use(pooledConn)` is wired, there is a window where the connection is back
in the pool and the guard is still armed on it, and a cancellation landing there
closes a connection the attempt no longer owns. `guard.release()` is now the
first statement inside the pool-disposition defer; the outer
`defer guard.release()` stays as the safety net for the two early returns that
precede it.

**Two neighbouring defects came out with it.** `applyTimeouts` ran only inside
`connect`, so a pooled client kept whatever `CommandTimeout`/`SubmissionTimeout`
its first dial set; and `recordPeer` never ran on the reused path, so the audit
row carried no peer IP. The old comment declined to claim an address "this
attempt never resolved" — with the socket carried through the pool that is no
longer a guess, and both now run in `acquire`.

`TestPool_ContextCancellationInterruptsAReusedConnection` was confirmed against
the unfixed code: it takes **60 s** (the test timeout) instead of returning
immediately.

**4. The MX cache needed a hard cap as well as a sweep.** Sweeping expired
entries on write does nothing for a workload that queries more domains than the
cache holds *within* one TTL window — every entry is live and nothing is
reclaimable. `maxCacheEntries = 1024` is a constant, not a configuration key, on
the reasoning `internal/attach` already applies to `maxParts`/`maxDepth`; past
it the map is dropped whole, keeping only the answer just stored. A cache is an
optimisation and being wrong costs one DNS lookup, which is cheaper than an
eviction policy that has to be explained.

**5. M11.6 was worse than "a stale reason".** `deferEnvelope`'s
`if reason != ""` guard means an envelope failing this way on its **first**
attempt got a completely **blank** `LastErr`, not merely an out-of-date one. The
fix is a three-armed switch rather than the plan's two, so a group whose members
have all vanished also says so. `targets` now returns `errors.Join` of every
member's resolution failure; the per-member `Warn` stays, because it names which
relay failed and the joined error does not.

Two counters were added, taking the golden snapshot-key list from 31 to **33**,
plus one gauge (`mailgw_failed_events_rejected`) — gauges do not ride the
heartbeat, which is correct: it is a depth, not an event.

### Accepted as-is

- **`LenAll` counts `.claim-<pid>-*.json` as pending** while
  `Replayer.Pending()` skips them. Pre-existing and left alone: an abandoned
  claim *is* parked work, so counting it is arguably the more honest answer, and
  changing a number nobody has complained about is not what this milestone is
  for.
- **The retention sweep rides the replay pass**, so `events.replay_interval: 0`
  disables both. A second ticker would have been the alternative;
  `startReplayer` logs a WARN naming the interaction instead, because a key that
  silently does nothing is the defect M10.4 exists to remember.

## Deliberately not done here

- **`dead/` retention.** It stays metadata-only and operator-emptied; that is
  the M7 decision and it is still right. Only `failed-events/rejected/` gets a
  sweep, because unlike `dead/` it has no CLI that lists it by default.
- **A spool disk quota.** `internal/queue/spool.go` has no size accounting, and
  adding one means deciding what a gateway does when it is full — refuse at
  MAIL, refuse at DATA, or stop accepting connections. That is a policy question,
  not a bound.
- **Per-IP and per-sender rate limiting.** M15.
