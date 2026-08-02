# M9 — Correctness and durability fixes

**Status:** **done** — M9.1–M9.4 (2026-07-29), M9.5 (2026-07-30, with M7), audited 2026-07-30  ·  **Packages:** `mailgw-go`, `webui-fastify`  ·  **Depends on:** —  ·  **Blocks:** M8

> Source: the code review of 2026-07-29 (`notes/review-2026-07-29.md` covers the
> earlier pass; findings 1–4 and 6 of the mailgw-go / Central Management pass are
> what this milestone closes).
>
> Numbered M9 so nothing above it moves — milestone numbers are identity, not
> running order.

From the review of 2026-07-29. Four independent defects; each task below is
self-contained and can land on its own. **M9.1 blocks M8** — attachment and
header rules are exactly the rules the bypass silences, so building them first
would build them onto a dead code path.

## M9.1 — data-stage recipient policy is skipped when routing resolved early — **done**

`internal/smtpsrv/session.go:538`. **Confirmed with a test**, not inferred:

```
policy: rcpt.domain == finance.example AND header.subject contains "secret" -> reject 550
routes: always -> relay Outbound
=> 250 2.0.0 Message queued (...)      # the rule matched and never fired
```

Change the route list so it cannot resolve at RCPT (add a higher-priority rule
reading a header) and the same policy rule returns `550 blocked by policy`. The
rule compiles correctly — `stage=data rcptScoped=true` — so this is purely the
early-decision path.

Cause: in `split()`, `EvalPolicyRcpt(renv, StageData)` sits inside
`if decision == nil`. One condition gates two unrelated things — **routing**
(safe to cache; `Route`'s stage-stop guarantees the early answer equals the late
one) and **policy** (not safe to cache; nothing was evaluated early). `Data()`'s
own `EvalPolicy(..., StageData)` does not cover it either: that call passes
`perRcpt=false`, so rcpt-scoped rules are filtered out of it by design.

Blast radius is wider than it looks: *any* route list that resolves at RCPT
triggers it, which includes **every transpiled `routing.json`** — the config
`mailgw-go/config/` actually ships. On a legacy-config deployment every
recipient-scoped data-stage policy rule is dead, silently.

- [x] Separate the two concerns in `split()`: build `renv` at `StageData` and
      run `EvalPolicyRcpt(renv, StageData)` **unconditionally**; use
      `rs.decision` only to skip the `Route` call.
- [x] Carry RCPT-time tags forward. `Route` at RCPT sets tags on the per-rcpt
      env and `Decision.Tags` captures that map; a freshly built data-stage
      `renv` would not see them, so the data-stage policy pass would evaluate
      `tag.*` against an empty map. Add `tags` to `rcptState`, seed `renv.Tags`
      from it. **Decide and write down the ordering rule** — policy-at-a-stage
      runs before routes-at-that-stage — because a cached decision inverts it.
- [x] Fix the `CountSoFar` inconsistency while in here: RCPT passes
      `s.rcptAccept` (recipients accepted *so far*), the DATA path passes
      `len(s.rcpts)` (the final count). Both are defensible; they must not
      disagree, or a `rcpt.count_so_far` rule matches differently depending on
      when routing happened.
- [x] Regression test in `internal/smtpsrv`, both directions: the bypass case
      **and** the late-route control that already passes. The control is what
      proves a fix did not simply disable early routing.
- [x] Confirm `explain -stage data` reports the same verdict the session now
      reaches, so `explain` stays a truthful preview.

Note for M7: a recipient refused by a data-stage rule still has no SMTP reply
left and is dropped with a WARN. This change makes that path *reachable more
often*, so the DSN item in M7 gets more valuable, but it is not a new problem.

## M9.2 — `data_timeout` is threaded end-to-end and never applied — **done**

`internal/deliver/client.go`. `server.yaml` → `config.Outbound.DataTimeout` →
`RunnerConfig` → `deliver.Options` → defaulted to 10m in `Deliver()` → and then
never read. Same for `ConnectTimeout` past the dial.

> **The original diagnosis was wrong in one respect, corrected during
> implementation.** The review said the post-connect phase was *unbounded*. It
> is not: go-smtp's `Client` carries its own `CommandTimeout` (5m) and
> `SubmissionTimeout` (12m) defaults (`client.go:108-112`). So a stalled relay
> wedged a per-group slot for **minutes, not forever** — still bad with
> `per_group_connections: 5`, but not the outage the review implied.
>
> The second correction matters more, because it invalidated the planned fix:
> **setting a deadline on the raw conn does not work.** go-smtp sets its own
> deadline around every command and *clears* it afterwards
> (`client.go:190-191`, `defer c.conn.SetDeadline(time.Time{})`), so an external
> deadline is wiped by the first command. The first implementation did exactly
> that and the new tests hung for 30s, which is how it was caught.

The real defect: the configured budgets were ignored and go-smtp's defaults
silently applied instead. The supported control is the client's own fields.

- [x] `applyTimeouts` sets `CommandTimeout` from `connect_timeout` and
      `SubmissionTimeout` from `data_timeout`, immediately after each client is
      constructed. `connect_timeout` covering a command round trip stretches the
      name slightly; both describe "how long we wait on this relay for one
      answer", and splitting them would mean a new config key.
- [x] **Do not reintroduce raw-conn deadlines.** `setDeadline` was written,
      shown not to work, and removed; the reason is recorded in the
      `applyTimeouts` doc comment so it is not tried again.
- [x] `connGuard` closes the live connection on context cancellation, armed
      **before the first dial** rather than after `connect` returns — the
      greeting and the STARTTLS handshake stall just as easily as the DATA
      phase, and the first version missed that (its test took the full 30s).
      go-smtp's client API takes no context, so closing the conn is the only
      way to interrupt a blocked command. Pairs with M9.5.
- [x] Tests: a relay that accepts and never answers, stalling both before the
      greeting and after EHLO, plus a cancellation test and a control asserting
      normal delivery still works. All bounded at ~300ms.

**Residual, accepted:** `NewClientStartTLS` builds its own client and runs the
greeting plus the handshake before we can reach it, and go-smtp exports no
`StartTLS` method to do it by hand. That one window keeps the 5-minute default.
Closing it means either an upstream change or reimplementing the STARTTLS dance,
neither of which is worth it for a window the `connGuard` already covers on
shutdown.

## M9.3 — `Reschedule` and `Bury` can duplicate mail across a crash — **done**

`internal/queue/spool.go:227,237`. The package doc says "Every state change is a
`rename(2)` … atomic". These two are not — they write the new file, *then*
remove the inflight one. A crash in between leaves the envelope in both `q/` and
`inflight/`; `Recover()` re-stamps the inflight copy's due-second to now and
moves it to `q/` under a **different** filename, and nothing dedupes. Two live
envelopes, message delivered twice. `Bury` is worse in kind: the duplicate is
resurrected out of `dead/`, so an envelope that was deliberately given up on
gets retried.

This is *not* the "crash mid-SMTP redelivers" case already documented as
inherent — that one is unavoidable, this one is created by the two-step write.

- [x] Dedupe in `Recover()` by envelope UUID. Filenames already carry it
      (`<due12>.<uuid>.json` in `q/`, `<pid>.<due12>.<uuid>.json` in
      `inflight/`, `<uuid>.json` in `dead/` and `quarantine/`), so this is a
      name scan — no file needs to be opened.
- [x] Build the seen-set from `q/`, `dead/` **and** `quarantine/`. When an
      inflight UUID is already present, delete the inflight copy rather than
      re-queueing it: the other copy is always the newer post-attempt state,
      because both writers commit the new location before removing the old one.
- [x] Correct the package doc. The honest claim is "no transition can lose an
      envelope; a crash mid-transition can leave a duplicate, which recovery
      resolves" — not "every transition is one atomic rename".
- [x] Test: place a `q/` and an `inflight/` file with the same UUID, run
      `Recover()`, assert one envelope survives and it is the `q/` one. Repeat
      for `dead/`.
- [x] While here, note but do **not** fix: `Complete` removes the inflight file
      before `gcBody`, so a crash between them orphans a body. That leaks disk,
      never mail — it belongs with the M7 refcount work.

## M9.4 — no DB transactions around multi-statement mutations (`webui-fastify`) — **done**

Central Management writes multi-row state with bare sequential statements, so a
failure part-way leaves the console in a state no operator asked for. Nothing in
`webui-fastify` uses `db.transaction()` today; Drizzle 0.45 + mysql2 supports it.

Worst case first — `CtrlGateway.saveAssignments` (`src/controllers/CtrlGateway.ts:168`)
deletes every assignment and then re-inserts them one at a time. A failure after
the delete leaves the gateway with **no** assignments, and the next Deploy
happily freezes `{server: null, routing: null, allowlist: {allowed: []},
relays: {}}` as a real, immutable, rollback-able version.

- [x] Wrap in `db.transaction()`: `saveAssignments`, `deployBundle` and
      `rollbackTo` (`src/central/bundle.ts`), and `CtrlGateway.deleteHandle`
      (four tables).
- [x] Collapse `saveAssignments`' insert loop into one multi-row insert inside
      the transaction.
- [x] **Validate the profile kind on assignment.** Adjacent gap in the same
      handler: any integer id is accepted for any slot, so a `ruleset` profile
      can be assigned to the `server` slot and its YAML lands in the bundle as
      `server.yaml`. The gateway fails closed on it (config error, keeps
      last-good, reports `apply_error`), so this is a footgun rather than a
      breach — check that `ConfigProfiles.kind` matches the slot.
- [x] Guard `+id` against `NaN` on the gateway routes while in the same file.
- [x] Keep `deployBundle`'s post-insert `SELECT` inside the transaction; the
      `uniq_version_gateway` index (migration 019) already prevents concurrent
      deploys from corrupting version numbering, so this is about partial
      writes, not the race.
- [x] Test: force a mid-sequence failure and assert the assignment set is
      unchanged. `setSessionStore()` already makes `app.inject()` tests run
      without a DB, so this needs the DB-backed path — note it as the first
      test that does.

Cross-referenced as item 17 in `webui-fastify/TODO.md`.

## M9.5 — shutdown always takes exactly 10 seconds — **done**

Three problems in the same dozen lines of `cmd/mailgw-go/main.go:404-416`. The
first is the review's finding 4; the second is the existing M7 shutdown bullet,
listed here because it is the same code and the two must be fixed together.
**M7's bullet stays where it is** — this is the detail, not a relocation.

> **Landed with M7** (`plans/M7-queue-completeness.md`), where the queue half of
> this already lived. One correction found on the way in: the first bullet was
> **already fixed** — the drain-versus-deadline select had moved into
> `gateway.shutdown()`, and this file was still pointing at `main.go`. Measured
> after the rest landed: **53 ms** on an idle gateway, against a 10 s grace
> period.

- [x] **The 10s wait itself.** `srv.Shutdown(shutdownCtx)` runs in a goroutine
      and the main path waits on `<-shutdownCtx.Done()`, which only fires on the
      timeout — so a gateway with zero open sessions still sits there for the
      full grace period. Docker's default stop grace is also 10s, so the
      container is SIGKILLed at the boundary on essentially every restart. Wait
      on whichever comes first:

      ```go
      done := make(chan struct{})
      go func() { _ = srv.Shutdown(shutdownCtx); close(done) }()
      select {
      case <-done:               // sessions drained
      case <-shutdownCtx.Done(): // grace expired
      }
      ```

- [x] **Wait for `Runner.Start` to return** (the open M7 bullet). Today the
      process can exit with a delivery attempt in flight; `Recover()` picks it
      up next boot, so it is cosmetic rather than lossy — but it is one more
      avoidable redelivery, and M9.3 shows how those compound. `Runner.Start`
      already ends with `r.wg.Wait()`; it just needs to signal completion (a
      `done` channel, or take a `*sync.WaitGroup`). **This only terminates once
      M9.2 lands** — today an attempt against a stalled relay never returns, so
      adding the wait first would hang shutdown rather than shorten it. That
      dependency is the reason this is not a standalone quick win.

- [x] **Order the teardown explicitly** rather than relying on `defer`: close
      listeners → drain SMTP sessions → cancel the runner → wait for the runner
      → `ev.Close()`. `ev.Close()` is currently a `defer` in `runServe`, so it
      does run last, but it can block up to 13s draining retries because
      `events.Client.deliver` sleeps on `context.Background()`. Give the client
      a shutdown context so in-flight retries abort and the remainder spills to
      `failed-events/`, which is what that directory is for.

- [x] Make the grace period configurable (`server.shutdown_timeout`, default
      10s) and document that `stop_grace_period` in both compose files must
      exceed it, or the ordering above is moot.

## Audit — 2026-07-30

Every checklist bullet above was re-verified against the tree. All five
sub-items are genuinely implemented; two gaps were found in what the checkmarks
*claimed*, both now closed.

**M9.1's last bullet was wrong.** "Confirm `explain -stage data` reports the same
verdict the session now reaches" was confirmed by hand, on one example — and one
example was not enough. `Ruleset.Explain` walked every policy rule at a stage in
one priority-ordered pass, but the session runs two: `Data()` takes the
message-scoped rules for the whole message, and only then does `split()` take the
recipient-scoped ones per recipient (`evalPolicy` filters each pass on
`RcptScoped`). Where a recipient-scoped data-stage rule outranks a matching
message-scoped one, `explain` reported a verdict the gateway would never reach —
exactly the "truthful preview" property the bullet exists to protect. The
precedence itself was never in doubt; it is written down at `session.go`'s
`dataEnvFor`. `internal/ruleset/explain.go` now does the same two passes per
stage, so the printed trace is in evaluation order rather than strict priority
order within a stage.

- [x] `TestExplain_AgreesWithTheSessionAtDataStage` — runs a real session and
      compares its reply to `Explain`'s verdict over the same facts, built the
      way `cmd/mailgw-go`'s `buildEnv` builds them. Fails before the fix
      (`explain` said 550/`rcpt-scoped-block`, the session answered 551).
- [x] `TestExplain_StillReportsARecipientScopedRule` — the control. Without it,
      "always prefer the message-scoped rule" would pass the test above.

**M9.5's `server.shutdown_timeout` was invisible.** Wired, defaulted and
diffed by `restartRequired`, but present in neither shipped sample, so an
operator raising `stop_grace_period` had no way to find the knob it must stay
ahead of.

- [x] `shutdown_timeout: 10s` in `mailgw-go/config/server.yaml` and
      `mailgw-go/testdata/config/server.yaml`, with the stop-grace constraint in
      the comment. The value equals `DefaultShutdownTimeout`, so nothing
      behavioural changes.
- [x] Asserted in `TestLoad_RealConfigDirectory`, which loads `testdata/config`.
- [x] `Server.validate()` deliberately left alone: `gateway.shutdownTimeout()`
      already reads a non-positive value as "use the default", and rejecting `0`
      would break that lenient reading.

Not closed here, noted instead: neither sample `server.yaml` carries a `tls:`
block either, so M8's inbound STARTTLS is undiscoverable the same way. Same
class of gap, different milestone.

**Not in M9, deliberately:** `/agent/register` has no rate limit
(`@fastify/rate-limit` is registered `global: false` and only `POST /login` opts
in), `POST /agent/report` trusts `applied_version_id`, and `bundle.ts`'s
`bodyOfKind` silently picks the first assignment of a kind. Real, but none of
them corrupt mail flow or config state — they belong with the M6 console work.

