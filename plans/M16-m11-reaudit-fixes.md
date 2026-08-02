# M16 — fixes from the M11 re-audit

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{smtpsrv,deliver,queue,events,config,obs}`, `cmd/mailgw-go`  ·  **Depends on:** M11  ·  **Blocks:** —

> Source: a re-audit of M11's own code, run on 2026-08-02 before it was
> committed. Nothing here is a new feature. Every item is a defect in code M11
> added or touched, or one it walked past.
>
> The premise was that M11 being green proved nothing, and it held: **every M11
> unit test constructs its subject directly, while three of its seven items only
> take effect through the wiring in `cmd/mailgw-go/listeners.go` and
> `cmd/mailgw-go/gateway.go` — which no test exercised at all.**

The headline is that M11.1 put a resource **leak** into the resource-bounds
milestone. On an `implicit_tls` listener the connection cap's wrapper hid the
`*tls.Conn` from go-smtp, which dropped the only pre-handshake read deadline in
the server, so a peer that completed TCP and then said nothing held its semaphore
slot for ever. `max.connections` such peers and the gateway answers `421` to
every real sender until it is restarted.

## M16.1 — the cap hid TLS from the server, and leaked slots

`Limit` wrapped every admitted connection so the slot could be released on
`Close`; `Guard` does not wrap (`internal/smtpsrv/listener.go:76`), so before M11
the `*tls.Conn` from `tls.NewListener` reached go-smtp intact. go-smtp finds TLS
by a bare type assertion with no unwrap interface (`conn.go:189`,
`server.go:164`). Behind the wrapper, on an `implicit_tls` listener:

- **Slots leaked.** `server.go:164-172` is the only place go-smtp arms a read
  deadline before the handshake; the other (`conn.go:1325`) is inside `readLine`,
  after `greet`. Skipped, the handshake ran lazily inside `greet →
  writeResponse`, which arms a **write** deadline only, while `crypto/tls` reads
  the ClientHello under the never-set **read** deadline. `inactivity_timeout`,
  which the sample config explicitly leans on, did not apply.
- **STARTTLS was advertised inside TLS** (`conn.go:259`), and `handleStartTLS`
  no longer short-circuited with "Already running in TLS", so a client taking the
  offer got a nested handshake. `REQUIRETLS` advertisement went with it.
- **TLS was wrong everywhere it is reported**: `Received:` said `with ESMTP`,
  the Connection audit row carried `using_tls: false`, and `conn.tls` /
  `conn.tls_version` / `conn.tls_cipher` were unset — so a policy rule requiring
  TLS **rejected every implicitly-TLS session**.

**Fix — split admission from accounting.** The cap is now two listeners:

```
tcp -> proxyproto -> Meter -> tls.NewListener -> Guard -> Limit -> srv.Serve
```

`Meter` sits **underneath** TLS and returns a `*slotConn` that is *unarmed*;
`Limit` keeps M11's position **outside `Guard`** — that argument was right and is
unchanged — but no longer wraps anything. It finds the `slotConn` beneath the
connection (direct assertion, else one hop through `NetConn()`, which `*tls.Conn`
has had since Go 1.18) and arms it. `slotConn.Close` releases once, under a
`sync.Once`, so the double-close protection M11 wrote survives. A chain with no
`Meter` in it falls back to M11's wrapping and logs at ERROR — the cap must never
silently stop bounding, even in a composition that should be unreachable.

## M16.2 — the refusal write stalled the accept loop

`refuse` wrote the `421` inline in `Accept` with only a **write** deadline set.
On a TLS listener that `Write` runs the handshake first, whose ClientHello read
was bounded by nothing, so one silent peer arriving while the cap was full stalled
that listener's accept loop indefinitely. `Guard.deny` had the identical shape
and the identical gap (pre-existing, since M8).

**Fix.** A shared `refuser` (`internal/smtpsrv/listener.go`) writes both the `550`
and the `421` on a bounded pool of goroutines — the shape
`internal/proxyproto` already uses — behind a full `SetDeadline`, closing the
connection before it calls back. Past `maxRefusalsInFlight` the reply is dropped
and the socket closed: a peer that gets no answer retries, an accept loop that
stalls serves nobody.

## M16.3 — a log line per refused connection

Both listeners logged at WARN once per refusal, on the accept goroutine, under
exactly the flood they were reporting. Now rate-limited to one line per 10s with
a `suppressed` count; `conn_denied` and `conn_throttled` stay exact.

## M16.4 — `guard.release()` did not disarm the guard

`release()` called `context.AfterFunc`'s stop and ignored the result. A `false`
return means the context has **already** ended and `closeConn` has already been
started on its own goroutine — it cannot be un-started, and at that moment it is
merely blocked on `g.mu`. So: attempt succeeds, ctx cancels, `release()` returns
"stopped" (it was not), `Pool.Put` publishes the still-open socket, another
worker takes it and starts a delivery, and the first attempt's `closeConn`
finally closes the socket underneath it.

M11's LIFO-defer note closed the *later*-cancellation window, not this
*concurrent* one, and `TestPool_ContextCancellationInterruptsAReusedConnection`
cannot see it because there the cancelled attempt sets `res.Err` and takes
`Discard`, never `Put`.

**Fix.** `release()` clears `g.conn` under `g.mu`. `closeConn` blocks on the same
mutex, so either it closes first — and the checkout RSET discards the dead entry,
which is what that probe is for — or `release` wins and there is nothing left to
close.

## M16.5 — `Pool.Put` did a network round trip under the mutex

Over `MaxPerRelay`, `Put` called `quietQuit` (QUIT **and wait for the reply**,
bounded only by `CommandTimeout`) with `p.mu` held. Every `Get`, `Put`, `expired`
and `Close` in the process serialised behind it, so one over-cap `Put` to a relay
that accepts TCP and then stops answering stalled the whole delivery path for up
to 30 seconds. The file already stated the rule — `expired()`'s own doc — and
broke it two functions above. Now: capture, unlock, close.

## M16.6 — the checkout probe was outside the guard

`acquire` ran before `guard.use`, and inside it `Pool.Get` issued `Reset()` and,
on failure, a polite QUIT. Neither was reachable by cancellation — the very
defect M11.5 set out to fix, still present on the half of the path that skips
`connect()` — and both ran before `applyTimeouts`, so they used whatever budget
the original dial had left. Against a black-holing relay with `MaxPerRelay` 5
that is minutes inside `acquire` before the caller even dials.

**Fix.** `Pool.Get` takes a `prepare` callback and runs it on each candidate
**before** probing it; `acquire` passes one that arms the guard and applies the
timeouts. A failed RSET now disposes with a hard `Close()` rather than a QUIT
that waits for a reply from a connection that just proved it may never answer.

## M16.7 — smaller delivery-path items

- **`MaxMessages` never fired on all-recipients-rejected envelopes.** The
  `len(accepted) == 0` path marked the connection poolable without counting the
  transaction, so a stream of such messages never retired a connection — and a
  relay's invalid-recipient cap is exactly what ends in the `421` `MaxMessages`
  exists to get ahead of.
- **`Pool.Close` was serial and outside the shutdown deadline.** It now takes the
  shutdown `ctx`, closes concurrently, and drops the sockets outright when the
  deadline passes: the descriptor matters, the courtesy does not.
- **MX domains are lower-cased** before lookup, so `Example.COM` and
  `example.com` stop taking two cache entries and two pool keys.
- **The equal-preference shuffle moved to read time.** Shuffling once per
  *lookup* fixed the order for the whole TTL, so RFC 5321 §5.1 spreading happened
  once an hour instead of once a message.
- **`outbound.connection_idle_timeout` is validated positive** when
  `reuse_connections` is on. An explicit `0` disabled `Get`'s lazy check,
  `expired()` and `Reap` all at once, and `Reap` exited with no log line while
  the gateway still reported that it was reusing connections.

## M16.8 — `LastErr` was still stale when the relay answered

M11.6 gave `targets` a joined resolution error and three switch arms. The fourth
combination — `last != nil && last.Err == nil`, the attempt where a relay **was**
reached and every recipient came back 4xx — had no arm, so `LastErr` kept
whatever the previous attempt left. A message that failed to connect once and is
then greylisted for a week reported "connection refused" in `mailq -json` for the
whole week; on a first attempt of that shape the field was blank, which is the
exact hole M11.6 was written to close.

`deferralReason` now summarises the attempt: the relay's own words when it gave
any ("451 4.7.1 greylisted, try again in 60s" is the whole answer to "why is this
still queued?"), otherwise a count and the relay's name. Empty when nothing was
deferred, so a finished envelope's reason is never overwritten with a non-event.
The DSN paths were insulated all along, because both prefer `rc.LastMsg`.

## M16.9 — the retention sweep aged files by spill time

`reject()` files a record with `os.Rename`, and **`rename(2)` does not touch
mtime** — it changes ctime. So `SweepRejected` measured age since the event was
*spilled*, not since it was rejected, and `Run` sweeps immediately after the pass
that rejected: an event spilled longer ago than `rejected_retention` was filed
under `rejected/` and deleted on the same tick. The evidence that directory
exists to preserve was destroyed before anyone could see it, with
`events_replay_failed` climbing while `mailgw_failed_events_rejected` stayed at
zero. `handleUnparseable` hit it every time, a torn file being old by definition.

Neither M11 test could catch it: both fabricate the mtime with `os.Chtimes`
instead of going through `reject()`. Now `reject()` stamps the file, and
`TestReplay_ARejectedEventSurvivesTheSweepInTheSamePass` rejects and sweeps in
one pass.

## M16.10 — abandoned claims were lost, and `Send` after `Close` could panic

**Claims.** A claim is the rename that locks one spilled event, and nothing
released one whose holder died between the claim and the outcome — SIGKILL past
the grace period being the ordinary way that happens. `Pending()` skips the name,
so the row was invisible to the CLI, to every later pass and to every counter,
while `Spool.LenAll` counted it for ever: a gauge that never returns to zero over
a listing that shows nothing. The "it will be sent again" log after a failed
`os.Remove` was simply false.

`Replayer.Reclaim` now returns a claim older than 15 minutes to the pending set,
and runs at the start of every pass. The claim time rides in the **filename**
(`.claim-<pid>-<unixnano>-<name>`), not the mtime: rename preserves mtime, and
stamping it would break `handleUnparseable`, which reads the same field to decide
whether a file may still be half written. A claim whose timestamp will not parse
is left alone — guessing at a format an older build wrote is how a file that is
genuinely in flight gets posted twice.

**Send after Close.** `Send` returned early on `c.closed` without counting
anything, on a path `gateway.shutdown` explicitly logs (the grace period expiring
with sessions still open). Those events are as unrecoverable as a buffer-full
drop and were the one loss missing from the counter whose whole point is that
those are the ones with no file on disk. Worse, `closed.Load()` and
`c.ch <- env` were not atomic against `close(c.ch)`: a goroutine that passed the
check a moment earlier would **send on a closed channel and panic the process**,
during shutdown, from the audit path.

`Close` no longer closes `c.ch` at all — senders stop on `done` and drain what is
buffered, and `Close` drains whatever raced in behind them, counting it. That
removes the panic window rather than narrowing it.

## M16.11 — counter and gauge HELP text that was not true

`events_replay_failed` and `mailgw_failed_events_rejected` both said flatly
"these are 4xx", but `reject()` is also reached by an unparseable record and by a
kind with no configured endpoint — so a corrupt file inflated a number an
operator reads as "logservice is rejecting our schema". Both now name all three
causes. `events_dropped` names the post-close drop it now counts. **No new
counters**, so the golden snapshot list stays at **33**.

## M16.12 — configuration and documentation

- `events.replay_interval` and `events.rejected_retention` reject a **negative**
  value. `Duration` parses `-720h` happily and everything downstream reads `<= 0`
  as "disabled", so a negative from a console textarea silently turned the pass
  and the sweep off — and the boot WARN only fires when retention is positive.
  Zero remains the documented off switch for both.
- `mailgw-go/README.md` said rejected events are "set aside rather than deleted".
  They have been swept since M11. It now documents the retention, that age counts
  from filing, the claim reclaim, and the two counter relationships an operator
  will otherwise misread: `conn_throttled ⊂ conn_accepted`, and
  `events_dropped` being the loss with no file behind it.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config
go run ./cmd/mailgw-go check -config ./config
```

All clean. **Every fix was confirmed against the unfixed code**, not merely
asserted: each new test was run with its own fix reverted and observed to fail —
including the slot leak, where `TestChain_SilentPeerDoesNotHoldASlotForEver` runs
to its 20-second bound rather than being greeted.

**Driven against a running binary as well**, since M16.1 and M16.2 are the items
with an externally visible protocol effect. A file-mode gateway with an
`implicit_tls: true` listener, a self-signed pair and `connections: 2`:

- `EHLO` inside the TLS session answered **without `STARTTLS`** (it offered it
  before the fix);
- a delivered message's `Received:` header read **`with ESMTPS`**, where it said
  `ESMTP` before;
- the third concurrent TLS session read `421 4.7.0 Too many connections, try
  again later` **decrypted**, and a fourth was greeted `220` once a held session
  ended;
- `SIGTERM` shut the process down immediately with the ordered teardown intact.

File mode is unregressed: `SMTP_PORT=2525 bun test tests/smtp` against that same
binary is **16 pass, 3 skip, 0 fail**.

New tests:

- **`cmd/mailgw-go/listeners_test.go`** — the production listener chain, which
  nothing covered before. It boots `smtpListeners.start` over a real
  `smtp.Server` with a probe backend and asserts: an implicit-TLS session is
  visible to the server as TLS, EHLO does not offer STARTTLS, the `421` is
  readable **inside** the TLS session, a slot returns when a TLS session ends, a
  silent peer does not hold one for ever, and a refusal does not stall the accept
  loop.
- `internal/smtpsrv/limit_test.go` — `Limit` hands on the `*tls.Conn` unwrapped
  and still finds the meter under it; an unarmed metered conn releases nothing;
  `serveCapped` now builds the shipped chain including `Meter`.
- `internal/deliver/guard_test.go` — `release` takes the connection away from the
  closer; an over-cap `Put` does not block the delivery path; a cancellation
  interrupts the checkout probe. All three against a "black hole" relay that
  answers TCP and the greeting and then nothing.
- `internal/queue/lasterr_test.go` — a deferring relay replaces the earlier
  reason, is recorded on a first attempt, and names the relay when it gave no
  message.
- `internal/events/reaudit_test.go` — reject-then-sweep in one pass keeps the
  file; an abandoned claim is reclaimed and a fresh one is not; `Send` after
  `Close` is counted and does not panic; `Close` twice is safe.

## What was built differently

Three things the audit's own plan did not anticipate.

**1. "Move the wrapper below TLS" is not one change, it is a split.** The obvious
fix — put the whole cap under `tls.NewListener` — reintroduces the defect M11
argued hardest about: admission would then happen before `Guard`, so a flood from
unlisted addresses could fill the semaphore and the cap would become the attack.
The parts have to go to opposite sides, which is only possible because
`*tls.Conn` exposes `NetConn()` and the slot can be *armed* after the connection
was *created*. The fallback path exists for the same reason M11's semaphore is
process-wide: a cap that silently stops counting is worse than no cap.

**2. The claim timestamp could not live in the mtime.** The first attempt at
M16.10 stamped the claim file, which is the natural mirror of the M16.9 fix — and
it breaks `handleUnparseable`, which reads that same mtime to decide whether a
file might still be half written. A stamped claim looks fresh for ever, so a
genuinely torn record would be deferred, unclaimed, restamped and deferred
again, without end. The time went into the filename instead, which is why
`parseClaim` exists and why it splits exactly two fields off the front — spill
names contain dashes of their own.

**3. Removing the `close(c.ch)` was the smaller change, not the larger one.**
The obvious fix for the panic is a mutex around `closed` and the send, on the
hottest non-mail path in the process. Not closing the channel at all removes the
window instead of guarding it: senders already had a `done` channel to select on,
and `Close` already waited for them, so the drain had somewhere to live. The
cost is that `run()` is a nested select rather than a `range`.

### Accepted as-is

- **`Spool.LenAll` still counts `.claim-*` files as pending**, which M11 recorded
  and left. With `Reclaim` in place the argument is stronger, not weaker: a claim
  is now genuinely transient parked work rather than a file nothing will ever
  look at again.
- **Reclaim can re-post an event whose removal failed after a successful POST**,
  producing a duplicate audit row. This pipeline is at-least-once everywhere
  else, and a duplicate row is much cheaper than a row nobody knows is missing.
- **`internal/deliver` still has no metrics or logger**, so nothing here counts
  reclaimed connections or refused pool checkouts. That is M10's decision and it
  stands: the signal rides out on `Result`.

## Deliberately not done here

Both of the first two are now **[M17](./M17-outbound-bounds-policy.md)**, which
exists precisely because they are questions rather than numbers.

- **A global cap on pooled connections.** Per-key is `MaxPerRelay`; the key space
  follows DNS when `use_mx` is on, so the ceiling is
  `MaxPerRelay × distinct exchangers seen within IdleTimeout`. With
  `connection_idle_timeout` now validated positive the reaper genuinely bounds
  it, and a second, global limit needs a policy for what to evict — which is a
  question, not a bound.
- **Negative caching for MX failures.** A DNS outage still costs one lookup per
  envelope per attempt. Adding one means deciding how long a failure is
  believed, which is the same class of question.
- ~~**Per-IP and per-sender rate limiting.** Still M15.~~ **Done** in
  **[M15](./M15-rate-limiting.md)**, which shipped five dimensions rather than
  the two named here — per IP, per sender, per authenticated user, per recipient
  domain and per failed AUTH.
