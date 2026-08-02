# M14 — Message authentication: SPF, DKIM, DMARC

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{msgauth,smtpsrv,queue,ruleset,config,obs}`, `cmd/mailgw-go`  ·  **Depends on:** M13  ·  **Blocks:** —

> Source: `mailgw-go/TODO.md:157-158` ("DKIM signing on relay (`go-msgauth`)",
> "SPF/DKIM/DMARC verification"). A case-insensitive grep for
> `spf|dkim|dmarc|arc` across the whole Go tree returns **zero hits** — this is
> absent, not partial.

## What is absent

- **Nothing verifies inbound.** No SPF check, no DKIM verification, no DMARC
  evaluation, no ARC.
- **Nothing signs outbound.** `internal/deliver/client.go:218` writes
  `msg.Body` straight through to the relay.
- **Nothing is recorded.** `session.receivedHeader()`
  (`internal/smtpsrv/session.go:974-992`) emits only `Received:` and
  `X-NGM-Gateway:` — no `Authentication-Results`, no `Received-SPF`. So even a
  downstream system that could act on the result is told nothing.

For a gateway whose only inbound gate is an IP allowlist (M13 adds the second),
this is the largest remaining feature gap. It is also the one most likely to be
demanded by a receiving domain rather than by an operator.

## Sequence

Each step is independently useful and independently shippable. Do them in this
order — later steps consume earlier results.

1. **Inbound SPF.** Cheapest: DNS only, no body access, evaluable at MAIL FROM.
   Produces a result (`pass`/`fail`/`softfail`/`neutral`/`none`/`temperror`/
   `permerror`) and nothing else.
2. **`Authentication-Results` and `Received-SPF` headers.** Prepended in
   `receivedHeader()`, which already strips CR/LF from everything it writes
   (`session.go:996-998`) — reuse that, do not write a second sanitiser.
   **Existing `Authentication-Results` headers from outside the trust boundary
   must be stripped**, or the header means nothing.
3. **DKIM verification.** Needs the body, so it belongs beside the attachment
   walk in `internal/smtpsrv/attach.go` — and inherits its constraint: the walk
   is a re-read of the spooled body, gated on `attach.enabled || NeedsMIME()`
   precisely so the default costs nothing (`mailgw-go/TODO.md:119-124`). DKIM
   verification must be gated the same way, on whether any rule reads a DKIM
   field, or it makes every message pay.
4. **DKIM signing on relay.** In `internal/deliver`, after routing, before the
   body is written. Note the ordering trap: **signing must happen after every
   header this gateway prepends**, or the signature covers a message that no
   longer exists.
5. **DMARC.** Consumes the SPF and DKIM results plus alignment; it is policy over
   two facts, so it is last and it is small.

## The architecture point

Every result becomes a **rule field** in `internal/ruleset/schema.go` —
`auth.spf`, `auth.dkim`, `auth.dmarc`, or whatever the registry naming settles
on — with its stage recorded so stage inference works (SPF is MAIL-stage, DKIM
and DMARC are DATA-stage). Then:

- policy stays in the DSL, not in Go;
- `mailgw-go fields` documents it for free;
- `mailgw-go explain` can answer "why was this rejected?" for it;
- a typo is a load-time error rather than a rule that never fires
  (`ruleset/schema.go` is the registry that makes that true).

This is exactly what M2 built the rule engine for, and it is why "reject on DMARC
fail" should be a rule an operator writes rather than a config boolean. Follow
M8's ordering precedent: **facts → rules → verdict**. Populate the fields before
the data-stage policy pass, and apply any built-in default action afterwards and
only when no rule reached a terminal action, so an `accept` rule is still the
whitelist.

## Library

`github.com/emersion/go-msgauth` is what `mailgw-go/TODO.md:157` already names,
and it is from the same author as `go-smtp` and `go-sasl`, both already direct
dependencies (`go.mod:7-8`). It covers DKIM, DMARC and `Authentication-Results`.
SPF needs a separate library or a hand-rolled resolver — `go-msgauth` does not
include one.

Weigh it in this file before adding: the module has six direct dependencies and
`plans/README.md` treats that as a constraint. The counter-argument is strong
here — DKIM canonicalisation is the kind of specification where a hand-rolled
implementation is wrong in ways that only show up as a receiving domain silently
rejecting mail. **Take the dependency for DKIM/DMARC; consider hand-rolling SPF,
which is a DNS walk with a lookup limit.**

## Keys

**A private key never travels in a configuration bundle.** This is the rule
established at `plans/M8-parity-hardening.md:96-106` and restated at
`mailgw-go/TODO.md:186-190`: the console stores every version forever and serves
it to every gateway on the profile, so a key placed there would be permanently
retained and fleet-wide.

So DKIM signing keys live in the gateway's data directory beside the TLS pair
(`/opt/mailgw-go/data/tls/` is the precedent; `.../dkim/` is the analogue), with
`0600` on the key and `0700` on the directory — the modes `internal/tlsx`
already sets (`tlsx.go:70,80`).

The **selector and domain** travel in the bundle; the key does not. A server
profile names the path. This has a real consequence worth stating plainly: **a
fleet cannot be given a signing key from the console**, so key distribution is
an operator problem, exactly as a real TLS certificate is today
(`mailgw-go/TODO.md:112-118`). Do not solve it by weakening the rule.

`internal/tlsx.EnsureSelfSigned` is the precedent for generating a keypair into
the data directory on a managed node — but do **not** copy it here. A
self-signed TLS certificate is a substitute for cleartext; a self-generated DKIM
key whose public half is not in DNS produces signatures that fail verification,
which is worse than not signing.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go fields          # the new auth.* fields, with their stages
```

- Fixture-driven, like `internal/dsn` (golden file) and `internal/attach`
  (`.eml` fixtures): a package that knows nothing about the spool, the session or
  the config, pinned by fixtures and shared by the session and `explain -eml`.
  That shape is why both of those packages are testable, and it is the shape to
  copy.
- A stub DNS resolver, so SPF and DKIM tests do not touch the network.
- **Sign-then-verify round trip** against a known-good implementation, not just
  against ourselves — a canonicalisation bug is self-consistent.
- `internal/deliver`: assert the signature covers the headers this gateway
  prepends, by verifying the exact bytes handed to the relay.
- Assert an inbound `Authentication-Results` from outside the boundary is
  stripped.

## Deliberately not done here

- **ARC.** It matters for forwarding, which is not what this gateway does.
- **Outbound DMARC reporting (`rua`/`ruf` aggregate reports).** A reporting
  pipeline is its own product; logservice already has the rows if it is ever
  wanted.
- **MTA-STS, DANE/TLSA, TLS-RPT.** Transport authentication, not message
  authentication — they belong with the M10.2 TLS work, and none of them has a
  consumer yet.
- **Rejecting on DMARC fail by default.** It becomes a rule field; what to do
  about it is the operator's policy, and a default that silently rejects mail
  would be the opposite of every other default in this gateway.

---

## What was built differently

Six departures from the plan above. Four are corrections to it.

**1. SPF is a library, not hand-rolled** (`blitiri.com.ar/go/spf v1.5.1`). The
plan said "consider hand-rolling SPF, which is a DNS walk with a lookup limit."
It is more than that — macro expansion with transformers, PTR, `exists`, the
10-lookup and 2-void-lookup budgets, and the RFC 7208 conformance suite — and
the library's `DNSResolver` is deliberately `*net.Resolver`-shaped, so it drops
straight into the same stub-resolver test story `go-msgauth`'s `LookupTXT` hook
uses. Its `Result` type is already the RFC 7208 §8 string set. **Direct
dependencies went 7 → 9**, and the second one is worth checking on an upgrade:
`gopkg.in/yaml.v3` is in its `go.mod` but used only by its conformance test, so
`go list -deps ./...` must keep showing it absent from the build graph.

**2. The results headers are `txnHeaders`, not `receivedHeader()`.** The plan
said "prepended in `receivedHeader()`". That cannot work: `receivedHeader()` is
concatenated ahead of the DATA stream *before it is read*, so the DKIM and DMARC
results do not exist yet. They ride on `txnHeaders` instead — the list
`add_header` already feeds — which gets the plan's actual instruction for free
("reuse that sanitiser, do not write a second one": `toQueueHeaders` →
`sanitizeHeaderValue`) and lands them **above** this gateway's own `Received:`
via `Envelope.PrependBlock`, which is the ordering RFC 7601 wants. The
consequence is worth stating: our own results are not in the spooled bytes, so
they are not in `msg.size` and not visible as `header.authentication-results` to
a rule. Both are correct — they are not a fact about the message we received.

**3. Signing is in `internal/queue`, not `internal/deliver`.** The plan put it
"in `internal/deliver`, after routing, before the body is written".
`deliver.Message.Body` is a one-shot `io.Reader`, and a DKIM signature has to be
written *ahead* of bytes the signer has already consumed — so the seam has to be
the caller that owns the spool and can open it twice. `queue.Signer` does that,
and it deliberately does **not** call `dkim.Sign`, which buffers the whole
message in memory to emit the signature first: two passes over a spool file beat
holding 25 MiB of RAM per delivery attempt.

**4. Only *our* `Authentication-Results` is stripped.** The plan said "existing
`Authentication-Results` headers from outside the trust boundary must be
stripped". RFC 7601 §5's actual requirement is narrower — remove fields bearing
*our* authserv-id — and the narrower rule is the better one: a gateway behind an
upstream that legitimately verifies mail should not destroy that upstream's
results, and a rule reading `header.authentication-results` can still see them.
The filter is installed only when this gateway will add a field of its own, and
is byte-for-byte identity otherwise, which is what makes it safe on the DATA path.

**5. The fields are `spf.*` / `dkim.*` / `dmarc.*`, not `auth.*`.** The plan
sketched `auth.spf`. Since M13, `auth.*` means "this **client** authenticated
with a credential we issued"; message authentication is a different claim about
a different party, and one prefix covering both would make `auth.authenticated`
ambiguous in exactly the rules that most need to be unambiguous.

**6. `explain` fakes the results rather than resolving them.** `-spf`, `-dkim`,
`-dmarc` and `-dmarc-policy` are flags on the same footing as `-tls` and
`-auth-user`. The question worth answering is "what would my rules do if DMARC
failed", not "what does DNS say right now", and it keeps `explain` free of
network I/O — so it runs on a laptop against a bundle for a gateway on another
continent.

### Two things the plan did not anticipate

**A check that did not run is ABSENT, not "none".** `none` asserts that a check
ran and found no policy. Every field reads as *missing* until its check has run,
so `spf.result eq "none"` cannot match a gateway that never looked, and
`FormatAuthResults` omits a method rather than claiming `none` for it. Without
this the header would state something the gateway never established.

**`DKIMResult.Domains` carries only signatures that VERIFIED.** A `d=` on a
broken signature is a claim anybody can attach; crediting it would let a forger
pass DMARC by bolting an invalid signature naming the victim's domain onto a
message. `TestEvaluateDMARC_OnlyPassingDKIMDomainsAlign` pins it.

### Known approximation

**There is no public suffix list**, so RFC 7489 §6.6.3's fallback to the
*organizational* domain is approximated: `lookupDMARC` walks up exactly one
label and refuses to land on a single-label parent, and relaxed alignment is
"equal, or one is a subdomain of the other". Both err the same way — they can
turn a pass into a `none` or a `fail`, never a fail into a pass — which is the
right direction for an approximation in a field nothing rejects on by default.
Mail from `a.b.example.com` inherits nothing from `example.com`'s record, and
`a.example.com` does not align with `b.example.com`. Adding a PSL would be a
megabyte of data and a tenth dependency; revisit it only if a real deployment
hits one of these.
