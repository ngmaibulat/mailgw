# M10 — SMTP correctness and delivery-path defects

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{smtpsrv,deliver,queue,config,obs,proxyproto}`  ·  **Depends on:** —  ·  **Blocks:** —

> Source: the audit of 2026-08-01, which read the shipped code paths rather than
> the backlog. Every finding below was confirmed in the source, not inferred —
> M9 is the precedent for why that distinction matters, since two of its own
> checkmarks turned out to claim more than the code did.
>
> Numbered M10 so nothing above it moves. It is worked **first** because these
> are defects in paths that carry live mail today.

Six independent fixes. Each is self-contained, each needs its own regression
test, and none depends on another — they can land in any order or separately.

What unites them: the gateway is correct on the paths it was tested on, and
wrong on the ones production supplies — an oversize message, a self-signed MX, a
load balancer in front, a hung scanner, a mail loop, a connection that never
reaches DATA.

## M10.1 — oversize messages answer `451` instead of `552`

`internal/smtpsrv/session.go:456-464`.

go-smtp's `dataReader` enforces `MaxMessageBytes` and returns
`smtp.ErrDataTooLarge` mid-read (`data.go:44-48`, `:71-78`) — a `*SMTPError`
carrying **`552 5.3.4`**, a permanent refusal. That error reaches us intact:
`bodyScan.Read` (`internal/smtpsrv/scan.go:34-44`) passes it through unchanged
and `Spool.WriteBody` wraps it with `%w` (`internal/queue/spool.go:113-115`), so
`errors.Is` already works. The handler simply does not look:

```go
name, size, err := s.b.Spool.WriteBody(s.txnUUID.String(), scan)
if err != nil {
    s.log.Error("cannot spool message", ...)
    return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0},
        Message: "Error: cannot queue message"}
}
```

Every failure — disk full, permission, size limit — becomes one indistinguishable
`451 4.3.0`. A `4xx` tells the sender to retry, so an over-size message is
retried for its whole queue lifetime and **can never succeed**; the sender
eventually gets a delay-expiry bounce that says nothing about size.

**Fix.** Branch on `errors.Is(err, smtp.ErrDataTooLarge)` and return
`552 5.3.4` naming `max.bytes`; keep `451` for genuine spool failures.
`WriteBody`'s `defer` already removes the temp file on any error path, so
nothing else changes. Note that `SIZE` is advertised from `max.bytes`
(`internal/config/config.go:465-468`), so a well-behaved client is refused at
MAIL FROM and this is the fallback for one that ignores it.

`smtp.ErrTooLongLine` is handled inside go-smtp's own reader (`server.go:192`)
and never reaches us — **confirm that by test rather than adding a branch** for
it.

**Test.** `internal/smtpsrv/contract_test.go`: set `Max.Bytes` low, send more,
assert `552`. There is currently no test anywhere in the module matching
`TooLarge` or `552`.

## M10.2 — opportunistic TLS silently downgrades to cleartext

`internal/deliver/client.go:322-330`, with `tlsConfigFor` at `:428-437`.

`tlsConfigFor` sets `ServerName` and leaves `InsecureSkipVerify` false, so
outbound certificates **are** verified. Then any `NewClientStartTLS` error —
x509 verification included — falls through to a cleartext redial at `:332-345`:

```go
// Opportunistic: fall back to an unencrypted session.
res.TLS = false
```

So against a self-signed MX, which is the common case rather than the exotic
one, encrypted delivery silently becomes plaintext. Against an active MITM the
same path strips TLS. The only trace is `res.TLS = false` on the audit event:
no counter, no log line.

`use_mx` widens it. `internal/deliver/mx.go:182-206` sets the synthetic relay's
`ServerName` to the exchanger's own name, so verification is attempted against
exactly the hosts least likely to pass it.

**Fix — two parts, and the second is the important one.**

- **Split the TLS config by policy.** `TLSOpportunistic` — the default, and what
  `Relay.TLSPolicy()` returns for an unset value
  (`internal/relays/relays.go:97-106`) — gets `InsecureSkipVerify: true`.
  RFC 7435 opportunistic security is encryption *without* authentication, and
  unauthenticated TLS is strictly better than the cleartext fallback this
  currently produces. `TLSRequired` keeps full verification, because that is
  what "required" has to mean, and it is the policy an operator sets when they
  have a relay whose certificate they trust.
- **Make the downgrade visible.** Add `deliver_tls_downgraded_total` to
  `internal/obs/metrics.go` and a `Warn` naming the relay and the reason
  whenever an opportunistic upgrade fails at all. The silent part is the defect;
  the verification change only decides how often it happens.

Also set **`MinVersion: tls.VersionTLS12`** on the outbound config. The inbound
side already pins it (`internal/smtpsrv/server.go:66`) and so does
`internal/central/client.go:83`; outbound is the one place that will still
negotiate TLS 1.0. **This can break a `tls: required` relay stuck on TLS 1.0** —
call it out in the release note rather than discovering it in the field.

**Test.** `internal/deliver/client_test.go` already stands up real go-smtp
instances as fake relays. A relay with a self-signed certificate must deliver
over TLS under `opportunistic`, must fail under `required`, and the downgrade
counter must move when STARTTLS is refused outright.

Adding a counter will fail `internal/obs/metrics_test.go` by design: the
reflection test requires every `atomic.Int64` field to appear in the name table
exactly once, and the golden snapshot-key count (28 as of M8) must be bumped
deliberately.

## M10.3 — the attachment scanner runs on `context.Background()`

`internal/smtpsrv/attach.go:69`:

```go
verdict, err := s.b.Attach.Check(context.Background(), s.txnUUID.String(), res.Parts)
```

This sits inside the DATA reply, so a hung logservice pins the SMTP session and
its goroutine — past `server.shutdown_timeout`, which M9.5 built an entire
ordered teardown around, and which M8 then had to fix again when it found the
event client holding `context.Background()` for its whole retry schedule. This
is the same defect in a different package.

The only bound is `Scanner.Timeout` (`internal/attach/scan.go:95-99,141-148`),
and `Server.validate` (`internal/config/config.go:443-500`) checks `attach.fail`,
`attach.url` and `attach.on_block` but **not** `attach.timeout` — so
`timeout: 0` makes both the `context.WithTimeout` and the `http.Client{Timeout}`
no-ops and the session hangs indefinitely.

**Fix.**

- Give `smtpsrv.Backend` a `Ctx context.Context`, set at bring-up in
  `cmd/mailgw-go/gateway.go` where the shutdown context already lives, and
  derive the scan context from it. `smtp.Session` has no context of its own —
  this is the only way in, and it is the same shape M8 gave `events.Client`.
- Validate `attach.timeout > 0` when `attach.enabled`, and clamp a zero to the
  default in `defaults()` regardless of the flag.
- Do the same for **`server.inactivity_timeout`** (`config.go:234,306`), which
  is likewise unvalidated and feeds `ReadTimeout`/`WriteTimeout` directly
  (`internal/smtpsrv/server.go:36-37`). An explicit `0` in a bundle is an
  unbounded slowloris, and a bundle is not a file an operator proofreads.

## M10.4 — `max.received_headers` and `max.header_lines` are dead config

`internal/config/config.go:93-94`, defaulted at `:302-303` — and read by
nothing. `NewServer` (`internal/smtpsrv/server.go:31-42`) sets
`MaxMessageBytes`, `MaxRecipients` and `MaxLineLength` only. Two greps confirm
it: each identifier has exactly two hits, the declaration and the default.

The consequence is that **there is no mail-loop guard.** `msg.received_count` is
computed (`session.go:474`) and exposed as a rule field
(`ruleset/schema.go:166`), so a loop is caught only if an operator hand-writes a
rule for it — and the config key that looks like it does the job does nothing.
A config key that silently does nothing is worse than an absent one.

**Fix — enforce both, rather than delete them.** RFC 5321 §6.3 requires a hop
limit, and these are the right names for it.

- `received_headers`: in `Data`, after `ReceivedCount` is computed, refuse over
  the limit with `554 5.4.6 Routing loop detected`. Count it.
- `header_lines`: enforce in `bodyScan.captureHeader`
  (`internal/smtpsrv/scan.go:52-70`), alongside the existing 1 MiB
  `maxHeaderCapture` cap, refusing with `552 5.3.4`.

Note the interaction with M10.1: both new refusals happen while the body is
being read, so both must return before or instead of the `WriteBody` error, and
the temp file must be cleaned up on each.

## M10.5 — no PROXY protocol, so the allowlist sees the balancer

`internal/smtpsrv/listener.go:102-116` (`addrOf`) and `session.go:116-119`
(`remoteIP`) read the TCP peer address and nothing else.

`internal/config/allowlist.go:14-17` states plainly that the IP allowlist is the
**only** inbound gate — there is no inbound AUTH (M13). So behind any L4 load
balancer or TLS terminator, every connection arrives from one address; allowlist
that address and the relay is open to everything behind it, and every `conn.*`
rule matches on the balancer. This is the gap most likely to be found by being
exploited rather than by being read.

**Fix.** Add `internal/proxyproto`: v1 text and v2 binary, hand-rolled, stdlib
only, ~150 lines. That matches how this module has treated every comparable
choice (`internal/obs` over `prometheus/client_golang`, `glob.go` over
`gobwas/glob`, `internal/store`'s own migrations) and `plans/README.md` names
minimal dependencies as a standing constraint. If the implementation grows past
what a reviewer will read, `pires/go-proxyproto` is the fallback — but say so in
this file rather than reaching for it quietly.

Parse **lazily on first `Read`**, not inside `Accept`, so one silent peer cannot
stall the accept loop, and put a deadline on the header read.

**Wiring** (`cmd/mailgw-go/listeners.go:60-83`). The PROXY header precedes the
TLS handshake, so the wrapper goes on the **raw** listener, *inside*
`tls.NewListener`, while `Guard` stays outermost:

```
tcp → proxyproto → tls.NewListener → Guard → srv.Serve
```

`tls.Conn.RemoteAddr` delegates down, so `Guard` and the session both see the
real client with no further change — the same delegation the existing comment at
`listeners.go:69-74` already relies on for the reverse reason.

**Config.** Per-listener `proxy_protocol: true`, plus a **`proxy_trusted` CIDR
list**: a PROXY header is trivially forged, so it is honoured only from named
peers and ignored (not trusted) from anyone else. **Fail closed** — if
`proxy_protocol` is set and the header is missing or malformed, drop the
connection. Both keys omit-when-empty so an unchanged bundle keeps its digest,
and `listen` is already on `restartRequired`.

**Test.** A real `net.Listen`, a v1 and a v2 header from a trusted CIDR, and
assertions that the allowlist sees the *forwarded* address, that an untrusted
peer's header is not honoured, and that a missing header fails closed.

## M10.6 — connection audit events fire only at DATA

`session.go:451` is the sole call site of `postConnection()`, and `Logout()`
(`:1088-1091`) posts nothing. So a connection that is greeted and dropped, RSET,
or has every recipient rejected produces **no `Connection` row at all** —
precisely the traffic an operator opens the log viewer to investigate.

Compounding it, `resetTxn` (`:1073-1075`) zeroes `rcptReject`/`rcptTempfail`,
which the code already acknowledges at `:365-368` ("a transaction ended by RSET
destroys its counts unseen"). A pipelining client that RSETs after rejections
loses the counts even when a row is eventually written.

**Fix.**

- Add `connPosted bool` to the session; `Logout()` posts if it is still false.
  `postConnection` reads only session-level fields (`:1000-1023`), so it is safe
  to call from there.
- Keep **session-cumulative** rcpt counters separate from the per-transaction
  ones `resetTxn` clears, and report the cumulative figures.

**Boundary, deliberately.** Peers rejected by the allowlist never reach
`Backend.NewSession` — the listener answers them itself
(`internal/smtpsrv/listener.go:64-88`). They stay metrics-only (`conn_denied`)
rather than moving event-posting into the listener, which would give the
listener a dependency on the event client and a UUID it has no other use for.

**Test.** `internal/smtpsrv/contract_test.go` for the in-process assertion, and
the Bun e2e (`MAILGW_DB_CHECK=1 pnpm test:e2e:smtp`) for the end-to-end one: an
aborted connection must now produce a `Connection` row where it previously
produced nothing.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...     # what .github/workflows/go.yml runs
go run ./cmd/mailgw-go check -config ./testdata/config
go run ./cmd/mailgw-go check -config ./config
```

Then the full stack, because 10.6 changes what the log tables receive:

```bash
pnpm certs && docker compose up -d
pnpm test:e2e:smtp:go
MAILGW_DB_CHECK=1 pnpm test:e2e:smtp
```

The DB check is what proves the `X` / `X.1` / `X.1.1` uuid hierarchy still
holds. `internal/smtpsrv/contract_test.go` ports every assertion from
`tests/smtp/tests/smtp.test.ts`, so a break there is a break in the Bun suite
too.

Any config key added here must appear in a bundle round-trip test
(`cmd/mailgw-go/bundle_test.go`) and, if it cannot hot-swap, in
`restartRequired` (`cmd/mailgw-go/gateway.go:696-729`).

## What was built differently

Five things the plan above got wrong or did not anticipate. Each was found by
writing the code or the test, not by re-reading the file.

**1. `smtp.ErrTooLongLine` DOES reach us, so the branch was not dead code.**
The claim below — "go-smtp answers it itself (`server.go:192`)" — is true only of
**command** lines. During DATA the line-limit reader sits underneath the reader
handed to `Session.Data`, so an over-long body line arrives as a spool error and
became the same `451` M10.1 exists to eliminate: a permanent format violation the
sender then retried for its whole queue lifetime. Proven by probe, then fixed in
the same `switch` as M10.1 — `500 5.5.2`, matching Postfix — and pinned by
`TestLimits_OverLongLineIsRefusedPermanently`.

**2. `internal/deliver` cannot count anything.** It imports no `obs` and holds no
logger; every delivery counter is incremented one layer up in
`internal/queue/runner.go` off the `*deliver.Result`. So `deliver_tls_downgraded`
is surfaced as `Result.TLSDowngraded` + `TLSDowngradeReason` — the way `Reused`,
`TLS` and `TLSForced` already are — and counted and logged in `runner.go`.

**3. M10.2's premise was understated, and there was a second bug next to it.**
Go completes the TLS handshake lazily, so a self-signed relay's certificate is
rejected on the *EHLO after STARTTLS*, not by `NewClientStartTLS` — and the
cleartext fallback only triggers on the latter. So the real pre-fix behaviour
against a self-signed MX was not a silent downgrade but a **failed attempt**, and
the message deferred and eventually bounced. Worse than described, and the same
fix resolves it. Separately, `res.TLS = true` was set *before* that EHLO, so an
attempt that never authenticated the peer was recorded as encrypted on the audit
row; it now moves after.

**4. M10.6's Logout post writes two rows per STARTTLS connection without a
guard.** go-smtp logs the pre-upgrade session out (`conn.go:948`) before calling
`NewSession` again, and each session mints its own `connUUID`. The session
records whether it *started* encrypted; a session that began in the clear and
finds itself on a TLS conn was superseded by the upgrade and stays silent.
`TestTLS_UpgradedConnectionReportsExactlyOneConnectionEvent` fails with two rows
if the guard is removed. A connection refused at the greeting also needed a post
from `Backend.NewSession`, since go-smtp never takes ownership of a session whose
creation returned an error and so never calls `Logout` on it.

**5. M10.5's "parse lazily on first `Read`" is self-contradictory**, and parsing
inside `RemoteAddr()` — what `pires/go-proxyproto` does — is no better. `Guard`
reads `RemoteAddr()` synchronously inside its own accept loop, so a lazily parsed
header is parsed *after* the allow/deny decision, leaving the allowlist looking
at the balancer. Doing it in `RemoteAddr()` fixes that but blocks the same
goroutine an inline parse would. Built instead as: **the trust check runs before
the first byte is read** (an untrusted peer costs one accept and one close, no
slot and no deadline), then a bounded pool of workers resolves headers
concurrently and `Accept` drains already-resolved connections.
`TestListener_ASilentPeerDoesNotStallOthers` is the test that distinguishes this
from the alternatives. The conn wrapper must return a **`*net.TCPAddr`**, because
`session.go:116` type-asserts it with no fallback.

At ~210 executable lines across two files the package stayed inside what M10 said
a reviewer would read, so `pires/go-proxyproto` was not needed — but the overrun
past the "~150 lines" estimate is the concurrency the lazy-parse sketch was
trying to avoid and could not, which M10 asked be stated here rather than
resolved quietly.

Three counters were added, not two: `deliver_tls_downgraded`, `msg_loop_rejected`
and `proxy_dropped`, taking the golden snapshot-key list from 28 to **31**.

## Deliberately not done here

- **`smtp.ErrTooLongLine` handling.** ~~go-smtp answers it itself; adding a branch
  would be dead code. Pinned by test instead.~~ **Superseded — see "What was
  built differently" above: it reaches us during DATA and is now handled.**
- **Streaming the MIME walk.** Still the M8 follow-up it was — measure first.
- **Rewriting over-long DATA lines.** Unchanged standing decision: Haraka's
  `\r\n ` injection breaks DKIM.
- **Direct-to-MX.** 10.2 touches `use_mx`'s TLS only. `use_mx` remains
  "smarthost named by domain"; consulting the recipient's own domain would need
  `split()` to bucket by domain, which is a milestone of its own.
