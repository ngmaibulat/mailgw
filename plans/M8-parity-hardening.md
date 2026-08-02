# M8 — Parity hardening

**Status:** **done** (2026-07-30)  ·  **Package:** `mailgw-go`  ·  **Depends on:** M2, M9.1  ·  **Blocks:** —

> Migrated verbatim from `mailgw-go/TODO.md` on 2026-07-29. This was the old M6,
> minus the metrics items that moved to M6.

## Goal

Close the last gaps between mailgw-go and the Haraka plugin set, so the legacy
gateway can be retired rather than kept alongside. Three things: attachment
scanning, inbound STARTTLS, and a replayer for spilled audit events.

**All three landed.** Nothing Haraka does is missing here now.

## Prerequisite — cleared

**[M9.1](./M9-correctness-and-durability-fixes.md) had to land before the
attachment work, and did (2026-07-29).** A recipient-scoped data-stage policy
rule was never evaluated when that recipient's route resolved at RCPT — and
attachment and header rules are *exactly* the rules that live at the data stage,
so the MIME walk would have been wired onto a code path that did not run for
most configurations, including every transpiled `routing.json`.

`internal/smtpsrv/policy_stage_test.go` guards that path in both directions;
`internal/smtpsrv/attach_test.go` adds the attachment cases it asked for.

## Done

- [x] Attachment MD5 scan, with both `AttachChecker.js` bypasses fixed first
- [x] Inbound STARTTLS, plus `implicit_tls`, which was parsed and validated but
      never applied
- [x] Replayer for `queue/failed-events/` — a background pass and a CLI

## Registry fields this unblocked

`msg.has_attachment`, `msg.mime_part_count` and every `attachment.*` field are
populated. Their entries are gone from `unpopulated` in
`cmd/mailgw-go/main.go`, so `check`, `fields` and startup no longer warn.

`auth.*` remains, pending inbound AUTH (Deferred, not part of this milestone) —
and **`mail.requiretls` joined it**. That field is permanently false, because
`EnableREQUIRETLS` is never set and go-smtp answers `504`, but it was not
declared anywhere: an undeclared dead field is precisely the failure the registry
exists to prevent.

## What was built differently from the plan

**The walk runs only when something will read it.** `internal/smtpsrv/scan.go`
argues at length against reading the spooled body back — it doubles the I/O and
gives the page cache a 25 MiB working set per concurrent session "for no
benefit". But a MIME walk needs the body, and `bodyScan` deliberately does not
buffer it, so the choice was a streaming multipart walker (an `io.Pipe`, nested
boundaries, transfer-decoding on the fly) or a re-read. The re-read won, gated on
`attach.enabled || Ruleset.NeedsMIME()` — a compile-time property of the rules.
The original objection was to paying for nothing; here there *is* a benefit, and
only for a configuration that asked for it. The shipped default pays nothing.

**The classification rule is one function, and a filename alone is enough.**
`AttachChecker.js:51` compared the disposition and nothing else, so
`Content-Disposition: inline; filename="invoice.exe"` was never hashed. The fix
is *not* "also scan inline parts when `include_inline` is set" — that leaves the
bypass open whenever the flag is off. A part that **names a file** is an
attachment whatever its disposition says, and `include_inline` only widens the
definition further, to nameless non-text leaves. `isAttachment` is a nine-case
table test for exactly this reason.

**`attach.on_block` was added, defaulting to `reject`.** The plan inherited
`npFilterAttach.js:45`'s DENYSOFT without comment. A 4xx makes the sender retry
for four days a message that will never be accepted: the digest is on a
blocklist and nothing about a retry changes that. `tempfail` is one key away for
literal parity, and `quarantine` reuses M7's release path.

**A scan *failure* answers 451, not `on_block`.** These are different events and
the legacy code conflated them, answering "allow" to both. A blocked attachment
is a verdict about the message; an unreachable scanner is a statement about this
gateway, and refusing permanently would turn ten minutes of logservice downtime
into mail that never arrives.

**The verdict is applied after the rules, not before.** Ordering is facts →
rules → verdict: the walk and the scan run first, so `attachment.*` and
`tag.attach_scan` are readable by the data-stage pass, and `attach.on_block` is
applied only when that pass reached no terminal action. An `accept` rule is
therefore still the whitelist, and a rule reading `tag.attach_scan` that chose
quarantine is not overridden. `attach.on_block` is the **default** answer to a
blocked message, not an override of one.

**`tls.starttls` became an opt-out.** It defaulted to false and was read by
nobody — go-smtp advertises STARTTLS purely off `TLSConfig != nil`, so a gateway
with a keypair offered it regardless and the flag was decorative. Defaulting it
true is what lets a gateway that has a certificate use it without being told to,
while `starttls: false` becomes expressible for a submissions-only node. Its
validation rule went with it: unlike `implicit_tls` it cannot require a keypair,
or every configuration that never mentions TLS would be rejected.

**Certificates never travel in a bundle; a managed node generates its own.** The
three options were a bundle format bump carrying PEM, an out-of-band mount, or
local generation. PEM in a bundle puts a private key in `ConfigVersions` forever
— immutable by design — and serves it to every gateway on the profile. A mount
breaks the zero-configuration property `deploy/gateway/` documents at length. So
`internal/tlsx.EnsureSelfSigned` writes a pair into the data directory, which is
already the durable bind holding the Ed25519 identity. It is not a substitute for
a real certificate; it is a substitute for cleartext, and opportunistic STARTTLS
with a self-signed certificate is what nearly every sending MTA accepts. An
operator who wants a real one drops it in the same directory — **no compose
change, no bundle change, no console change**.

**Certificate reload was added, unasked.** `restartRequired` compares cert
*paths*, never contents, and the keypair was loaded exactly once at bring-up — so
a 90-day certificate renewed in place kept serving the expired one until somebody
noticed. `tlsx.Reloader` stats both files per handshake and reloads on an mtime
change, and keeps the last good pair when a rewrite is caught half-done.

**`Guard` wraps the TLS listener, not the other way round.** Either nesting
compiles. Inside-out writes the allowlist's `550 Access denied` as cleartext onto
a socket expecting a handshake, so the peer sees a protocol error instead of the
reason. `tls.NewListener`'s `Accept` does not handshake and `tls.Conn.RemoteAddr`
delegates, so putting `Guard` outside costs nothing and the denial is readable.

**The spill format was hardened before anything read it.** `events.Client.spill`
used `os.WriteFile` — not atomic — with a `%d.<kind>.json` name written by four
concurrent goroutines. A replayer scanning a directory a live gateway is writing
into would eventually read half a record, and two spills in one nanosecond
silently overwrote each other. Now: tmp+rename, a fixed-width 19-digit
nanosecond (so a lexical sort is chronological — the same trick `dueWidth` plays
in the spool) and a sequence suffix.

**Claim-by-rename, because two replayers is the ordinary case.** The gateway's
background pass and `mailgw-go events replay` can run at the same moment. Each
file is renamed to a claim name before it is posted — the same "the rename is the
lock" idiom as `Spool.Claim`. A test runs four concurrent passes over 25 events
and asserts logservice saw each exactly once.

**A replay follows the current configuration, not the record.** Each spilled
event carries the URL it was originally posted to, and using it would be the
obvious choice — but a managed gateway's logservice URL arrives in a bundle and
changes with it, so a stale address would be retried forever. The recorded URL is
the fallback, not the source of truth.

**A pass gives up after three consecutive transport failures.** The normal reason
a spill directory is full is that logservice is down, and marching a thousand
events into a closed socket helps neither side. A 4xx is different and terminal:
it goes to `failed-events/rejected/`, set aside rather than deleted, because it
is the evidence of what was refused.

**`events.Stats` finally has a reader.** Seven `atomic.Int64` counters in
`internal/events` were reachable from nowhere. `Options.Metrics` now feeds `obs`,
adding `events_spilled`, `events_replayed` and `events_replay_failed` plus a
`mailgw_failed_events` gauge — so "is the audit trail behind?" is answerable from
`/metrics` and from the console heartbeat.

## A shutdown defect the replayer's tests exposed

**`Close`'s deadline did not bound the event already in flight.** `run()` read
the shutdown context **once per event**, before calling `deliver`, so a sender
that had already picked an event up when `Close` was called held
`context.Background()` for that event's entire retry schedule — six attempts and
five backoffs, ~37 seconds on the shipped defaults — while `gateway.shutdown`
waited on it, well past `server.shutdown_timeout`. Cancelling the backoff was not
enough either: a sleep that had already started was sleeping on `Background`.

This is M7/M9.5 code, not M8's, and it only surfaced because the new replay tests
changed the package's scheduling enough to make an existing flake reproducible.
Two changes: `deliver` re-reads `drainCtx()` **per attempt**, which bounds the
window to the single HTTP attempt in flight; and `Client.done` is closed by
`Close`, so a backoff already sleeping wakes immediately.
`TestClose_BoundsTheEventAlreadyInFlight` pins it and takes 50s to fail without
the fix.

## Two things found while building

**`postQueue` never set `mime_part_count`.** The field has been on the queue
payload since the Haraka days (`functions.js:171`), logservice has a column for
it, and this gateway had always sent `0`. It is populated now — but only when the
walk ran, because a message nothing walked has no known part count, and a
fabricated `0` is the same lie `Gauges.QueueOK` exists to avoid.

**`explain -eml` carried an IOU in a comment** (`cmd/mailgw-go/env.go:56-61`)
saying it would not invent attachment values while `serve` did not populate them.
It now calls the same `attach.Walk` with the same `include_inline` from the same
configuration, so the preview and the gateway cannot disagree.

## Verified

`go build ./... && go vet ./... && go test -race ./...` clean; `check -config
./testdata/config` reports no unpopulated-field warnings; the **Bun SMTP suite
runs unmodified** against the binary (16 pass, 3 skip). End to end against a
running gateway: a blocked digest answers `550 5.7.1 Blocked: attachment scan`
and a clean one delivers, with the descriptor posted in the exact legacy shape
(`md5`/`contentType`/`filename`/`size`/`txn_uuid`, over decoded bytes); STARTTLS
on 2525 and implicit TLS on 2465 both negotiate TLS 1.3; a queue event carries
`mime_part_count: 3` for the first time; and 11 spilled events drained themselves
on the background pass, showing `mailgw_events_replayed_total 11` on `/metrics`.

> `/healthz` and Prometheus metrics moved to **M6** — they are how a fleet
> console tells a healthy gateway from a wedged one, so they were no longer the
> last thing on the list.
