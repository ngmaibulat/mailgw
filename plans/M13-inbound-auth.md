# M13 — Inbound SMTP AUTH

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{smtpsrv,config,ruleset,obs,dsn,queue}`, `cmd/mailgw-go`, `webui-fastify`, `logservice/migrations`  ·  **Depends on:** M8 (inbound TLS)  ·  **Blocks:** M14

> Source: `mailgw-go/TODO.md:156` ("Deferred: Inbound AUTH"), and the
> `unpopulated` registry at `cmd/mailgw-go/main.go:187-191`, which has been
> telling operators the truth about this since M8.

## What is missing, and how the code says so

`internal/smtpsrv/session.go:100` asserts only:

```go
var _ smtp.Session = (*session)(nil)
```

go-smtp advertises AUTH solely when a session also implements
`smtp.AuthSession` (`backend.go:95-102`):

```go
type AuthSession interface {
    Session
    AuthMechanisms() []string
    Auth(mech string) (sasl.Server, error)
}
```

It does not, so `AUTH` is never advertised and `Server.AllowInsecureAuth` is
never set (`internal/smtpsrv/server.go:28-47`).

The consequence is the one `internal/config/allowlist.go:14-17` already states:
**the IP allowlist is the only inbound authorization gate.** There is no
submission-with-credentials path at all, and after M8 an `implicit_tls` listener
on 465 accepts unauthenticated mail from any allowlisted address — a submission
port with no submission auth.

`HeloEnv.AuthUser` and `HeloEnv.AuthMechanism` (`internal/ruleset/env.go:88-89`)
are declared, read by the evaluator (`env.go:256-265`) and exposed in the schema
(`ruleset/schema.go:142-143`) — and **written nowhere**. A grep for `AuthUser`
under `internal/smtpsrv/` returns nothing. They are two of the three entries in
`unpopulated`, so `check`, `fields` and startup warn rather than letting a rule
silently never match. **Removing them from that map is this milestone's
completion signal.**

## M13.1 — implement `smtp.AuthSession`

`github.com/emersion/go-sasl` is **already a direct dependency** (`go.mod:7`),
pulled in by the outbound side, so this adds no new module — worth stating,
because `plans/README.md` names minimal dependencies as a standing constraint.

Mechanisms: `PLAIN` and `LOGIN` cover every client that will meet this gateway.
Do not implement `CRAM-MD5` — it requires storing recoverable passwords, which
would be a worse decision than the one it saves.

Design points, each of which has a shipped constraint behind it:

- **`AllowInsecureAuth` defaults off.** M8 shipped inbound STARTTLS as an
  opt-out (`tls.starttls`, default true), so an encrypted session is available;
  advertising PLAIN in cleartext by default would undo that. It stays
  configurable, because an operator terminating TLS in front of the gateway has
  a legitimate case — and after M10.5 that operator has PROXY protocol, so the
  gateway can at least see who is connecting.
- **AUTH state must survive the STARTTLS upgrade.** go-smtp calls
  `Backend.NewSession` a **second time** after an upgrade — M8 discovered this
  and pinned it with `TestTLS_PolicyIsReevaluatedOnceAfterTheUpgrade`
  (`internal/smtpsrv/tls_test.go`). Authentication happens after the upgrade in
  a correct client flow, so this should be a non-issue; assert it rather than
  assume it.
- **Failed AUTH must be counted and rate-limitable.** Add the counter here; the
  limiter is M15. Without a counter, a credential-stuffing run against the relay
  is invisible.

## M13.2 — where credentials come from

A managed node is zero-configuration: no environment, no files, no arguments. So
credentials travel in the config bundle, the same way `logging.api_key` and
per-relay `auth_pass` already do (`plans/M5-config-pull.md:185-201`).

That inherits M5's decision and its consequence:

- The console holds them encrypted at rest via `CONFIG_SECRET_KEY`
  (`webui-fastify/src/central/secrets.ts`) and decrypts when composing a bundle,
  so no key reaches a gateway.
- **The bundle at rest in the gateway's SQLite is plaintext** — stated at
  `plans/M5-config-pull.md:305-311`. That is already true of relay passwords, so
  this changes the blast radius, not the property.

Store **hashes, not passwords** — bcrypt or argon2id, matching whatever
`webui-fastify` already does for `Users.hash` (`bcryptjs`). The bundle then
carries hashes and the plaintext-at-rest concern above narrows to something that
does not matter. Do this even though it costs a console-side schema addition; it
is the difference between a leaked bundle being an incident and being a
catastrophe.

Console side: a new profile kind or a table beside `Relays`, with the usual
omit-when-empty treatment so an unchanged configuration keeps hashing identically
(`webui-fastify/src/central/bundle.ts`).

File mode keeps working unchanged, reading the same shape from a config file.

## M13.3 — the rule fields

Populate `auth.user` and `auth.mechanism` on the session env at the point AUTH
succeeds, and **remove both from `unpopulated`**
(`cmd/mailgw-go/main.go:187-191`). Add `auth.authenticated` as a boolean if the
registry does not already imply it — `exists` over `auth.user` covers it, but an
explicit field reads better in a policy rule and the registry is where field
names are decided.

The point of doing this in the rule engine rather than in Go: "authenticated
senders may relay anywhere, everyone else is allowlist-only" becomes a policy
rule an operator writes, not a branch in `session.Rcpt`. That is the architecture
M2 exists for.

## M13.4 — the other two `unpopulated` entries

`NewServer` (`internal/smtpsrv/server.go:31-42`) never sets `EnableDSN`,
`EnableREQUIRETLS` or `EnableBINARYMIME`. Three consequences, all currently
invisible:

- **No RFC 3461 DSN support inbound.** `NOTIFY=`, `ORCPT=` and `RET=` are
  answered `504` (go-smtp `conn.go:395-425`). A gateway that *generates* DSNs
  (M7, `internal/dsn`) but cannot *honour* a sender's DSN request is a real
  asymmetry, and `ORCPT` is what lets a bounce name the address the sender
  originally used. Worth closing here.
- **`mail.requiretls` can never be true** — set at `session.go:291` from an
  option go-smtp rejects at `conn.go:372-375`. It is the third `unpopulated`
  entry. Advertising REQUIRETLS is a promise about *outbound* delivery this
  gateway does not currently keep (relay TLS is per-relay policy, and M10.2
  makes opportunistic explicitly unauthenticated), so **leave it declared as
  unpopulated** and say so — that is the honest answer, not an oversight.
- **`mail.body == "BINARYMIME"` is dead code.** `schema.go:152` documents it and
  `session.go:287` tests for `smtp.BodyBinaryMIME`, but `EnableBINARYMIME` is off
  so `conn.go:382-384` answers `504`. It is **not** in `unpopulated`, so `check`
  does not warn about it — the one gap in that map's coverage. Either enable
  BINARYMIME or add it to `unpopulated`; the second is cheaper and probably
  right, since BDAT already routes to the same `Session.Data`.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go fields          # must no longer warn about auth.user / auth.mechanism
go run ./cmd/mailgw-go check -config ./testdata/config
```

- `internal/smtpsrv/contract_test.go`: AUTH is advertised only when configured;
  PLAIN over an unencrypted session is refused with `AllowInsecureAuth` off;
  a successful AUTH populates `auth.user` for a policy rule; a failed AUTH is
  counted.
- `internal/smtpsrv/tls_test.go`: authentication survives the second
  `NewSession` after STARTTLS.
- `internal/ruleset`: a rule reading `auth.user` compiles without a warning and
  fires.
- `cmd/mailgw-go/bundle_test.go`: credentials round-trip in a bundle and are
  omitted when empty, so an existing configuration's digest does not change.
- Console side: `webui-fastify` needs its own test that a profile without
  credentials composes a byte-identical bundle to today's.

## What was built differently

Eight things. The first is a correction to this plan rather than an elaboration
of it, and the last two are defects this milestone found in already-shipped code.

**1. `auth.user` and `auth.mechanism` could not stay at `StageHelo`.** This plan
took the existing schema entries as correct and only proposed populating them.
They were at the wrong stage: `greetPolicy` (`internal/smtpsrv/session.go`)
evaluates `StageConnect` **and `StageHelo`** inside `Backend.NewSession`, and
go-smtp refuses to process `AUTH` until EHLO has been answered
(`conn.go:819-822`). So a rule inferred to the helo stage would have been
evaluated before the client had any opportunity to authenticate, and could never
have matched — the exact failure the schema registry exists to prevent, arriving
by way of the fix for it. All three fields (including the new
`auth.authenticated`) sit at **`StageMail`**, the earliest stage at which the
answer is reliable. Nothing else moved: stage inference is table-driven, and
`resetTxn` never clears `env.Helo`, so an identity survives RSET for free.

**2. go-sasl has no LOGIN server.** The plan noted that `go-sasl` is already a
dependency and concluded LOGIN was free. Only the *client* half is there
(`login.go`); `NewPlainServer` exists but `NewLoginServer` does not. The server
half is ~40 lines in `internal/smtpsrv/auth.go`. The plan's actual claim — that
this milestone adds no new SASL dependency — still holds.

**3. `allow_insecure_auth` lives in `server.yaml`'s `tls:` block**, not beside
the credentials. It is a property of the listener rather than of any user, it
mirrors `relays.Relay.allow_insecure_auth` on the outbound side, and being part
of `TLSConfig` puts it on `restartRequired`'s existing `tls` entry for free —
which is correct, because go-smtp reads it once when the server is built. The
credentials themselves hot-swap.

**4. A bad credential answers `535 5.7.8`, not go-smtp's `454`.** `handleAuth`
writes a flat `454 4.7.0` for any authenticator error, which tells a client the
failure is temporary and invites it to retry the same wrong password. Returning
an `*smtp.SMTPError` gets it passed through verbatim (`conn.go:1314-1320`) — the
same mechanism `Session.Data`'s deliberate `250` already relies on.

**5. The plan's BINARYMIME suggestion was declined.** M13.4 proposed adding
`mail.body` to `unpopulated`. That map is keyed by **field name**, and
`mail.body` *is* populated — it carries `7BIT` and `8BITMIME` today — so an
entry there would warn on every working `mail.body` rule, which is the opposite
of what the map is for. The field's `Desc` says instead that `BINARYMIME` can
never appear. Enabling the extension was rejected separately: it makes go-smtp
require BDAT and block DATA, which needs a body path that does no dot-stuffing.

**6. ORCPT and ENVID had to be re-encoded on the way out.** go-smtp xtext-decodes
both on the way in and does not export an encoder, so `internal/dsn` grew one.
It is not cosmetic: `+` is an xtext special and `user+tag@example.com` is an
ordinary address, so emitting it raw produces a value the receiving system
decodes into something else. Note go-smtp's own unexported encoder formats
without zero-padding, so a byte below `0x10` emits `+9` where xtext demands
`+09`; this one does not copy that.

**7. `Original-Envelope-Id` was carrying the wrong value.** Pre-existing, and
invisible until now because nothing could send `ENVID`. `dsn.Report.OriginalID`
is this gateway's envelope uuid and was being emitted as `Original-Envelope-Id`,
which RFC 3464 §2.2.1 defines as *the sender's* ENVID. A receiving system
matching that field against what it sent would have got a confident non-match
against something that looked like an envid. The two are now separate: the
sender's ENVID is `Original-Envelope-Id`, omitted when absent, and the gateway's
own identifier moved to `X-NGM-Original-Envelope` beside the `Received` and
`References` it was already in. **This is the golden file's only shape change.**

**8. Two notifications about one envelope overwrote each other's body.** Also
pre-existing. `Bounce` named the notification's body for the envelope it reports
on (`data/<parent>.eml`), so a delay warning at four hours and a failure at four
days wrote the same file — and if the warning was still queued, its sender
received the failure report twice, once under an envelope claiming to be a delay
warning. NOTIFY=SUCCESS made it common rather than rare: a message with one
delivered and one rejected recipient produces a failure report and a relayed
report in the same attempt. The body is now named for the notification itself,
which needed the body-ownership rule relaxed from "an ancestor's name" to "its
own or an ancestor's" — in `Envelope.validate` **and** in `Spool.bodyReferenced`,
which uses the same rule as a shortcut to avoid opening every candidate file. A
`bodyOwnedBy` helper is shared by both, because if those two ever disagreed the
disagreement would delete a live body.

## Deliberately not done here

- **CRAM-MD5 and other challenge mechanisms** — they require recoverable
  passwords.
- **REQUIRETLS.** Declared unpopulated, deliberately; see M13.4.
- **Per-user rate limiting on failed AUTH.** Counter here, limiter in M15.
  `internal/smtpsrv/auth.go` bounds concurrent bcrypt comparisons to GOMAXPROCS
  so a credential-stuffing run cannot starve the delivery runner of CPU, but
  that is a floor under the damage, not a limiter.
- **OAuth / XOAUTH2.** No consumer.
- **Propagating DSN parameters to the next hop.** `internal/deliver` sends
  `MAIL FROM` and `RCPT TO` with no DSN parameters, whether or not the relay
  advertises `DSN`. RFC 3461 §5.2.7 anticipates this and its remedy is that the
  gateway takes responsibility for the recipients it accepted — which is what
  this one does, since it spools, retries and answers for them itself. Three
  consequences an operator should know: `NOTIFY=NEVER` binds this gateway only,
  so a failure *downstream* still produces a bounce from that MTA to the
  unchanged envelope sender; a downstream DSN carries no `Original-Recipient`
  (which loses nothing here, because this gateway does no address rewriting, so
  `Final-Recipient` downstream is the address the sender used); and it carries
  no `Original-Envelope-Id` and returns whatever that MTA's own policy chooses.
  Propagation is a strict improvement rather than a prerequisite, and it ripples
  through `deliver.Message`, four test files and two fakes — its own item.
- **Success DSNs beyond "relayed".** `Action: delivered` is reserved for a final
  delivery, which only the destination system can report. This gateway knows a
  relay accepted the recipient and nothing about what happened afterwards.
