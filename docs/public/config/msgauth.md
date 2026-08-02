# Message authentication

SPF, DKIM and DMARC on the way in; DKIM signing on the way out.

```yaml
msgauth:
    spf:
        enabled: false
    dkim:
        enabled: false
    dmarc:
        enabled: false
    authserv_id: ""
    max_dkim_signatures: 10
    dns_timeout: 5s
    sign:
        enabled: false
        keys: []
        canonicalization: relaxed/relaxed
        headers: []
        expiration: 0
```

Everything here is **off by default**, and turning a check on changes what every
message costs — the same treatment `attach.enabled` and
`outbound.reuse_connections` get.

::: tip Nothing here rejects anything
Every result becomes a [rule field](/rules/fields): `spf.result`, `spf.domain`,
`dkim.result`, `dkim.domains`, `dmarc.result`, `dmarc.policy`. What to do about
a DMARC failure is a rule you write. There is no `on_dmarc_fail` key, and
turning these on cannot start refusing mail on its own.
:::

## You may not need to set anything

A rule that reads `spf.*`, `dkim.*` or `dmarc.*` turns the matching check on by
itself. These flags exist for the other case — wanting the
`Authentication-Results` header without a rule to go with it.

```yaml
# This alone makes every message pay for an SPF lookup. No msgauth: block needed.
policy:
    - name: refuse SPF failures
      match: {field: spf.result, op: eq, value: fail}
      then:
          - {action: reject, code: 550, enhanced: "5.7.23", message: "SPF fail"}
```

`mailgw-go check` prints which checks are on and which a rule turned on:

```
  msgauth:  verifying spf, dkim (turned on by a rule), dmarc as "mx1.ngm.dev"
```

A `dmarc.*` rule turns SPF and DKIM on too, because DMARC is alignment over
their results. Setting `dmarc.enabled` with neither `spf` nor `dkim` enabled is
a configuration error rather than a silent every-message failure.

## When each check runs

| Check | Stage | Costs |
|---|---|---|
| SPF | `MAIL FROM` | up to 10 DNS lookups |
| DKIM | end of `DATA` | one DNS lookup and one signature verification **per signature**, plus a re-read of the spooled message |
| DMARC | end of `DATA` | one or two DNS lookups |

SPF is answerable at `MAIL FROM` because it never looks at the message — which
is what lets a rule refuse a failing sender on its own `MAIL` line rather than
after a megabyte of `DATA`.

`max_dkim_signatures` bounds how many signatures on one message are checked.
Each costs a lookup and a verification, so an unbounded count is an
amplification primitive pointed at your gateway. Signatures past the bound are
ignored; the ones checked are still reported.

## Headers

When any check runs, two headers are added to the message on its way out:

```
Authentication-Results: mx1.ngm.dev; spf=pass smtp.helo=mail.partner.com
  smtp.mailfrom=alice@partner.com; dkim=pass header.d=partner.com;
  dmarc=pass header.from=partner.com
Received-SPF: pass (mx1.ngm.dev: domain of partner.com designates 203.0.113.7
  as permitted sender) client-ip=203.0.113.7; helo=mail.partner.com;
  envelope-from=alice@partner.com;
```

A check that **did not run** is omitted rather than reported as `none`: `none`
asserts that the gateway looked and found no policy, and claiming it for a check
that never happened would be a lie in the one header whose purpose is to be
believed downstream.

### authserv_id

The name this gateway signs its results with. Empty means `hostname`.

It is also the name under which **forged results are stripped**. RFC 7601 §5
requires any system that adds an `Authentication-Results` header to remove
inbound ones claiming its own identity — without that, a sender simply asserts
`dkim=pass` under your name and nothing downstream can tell.

Only fields bearing *your* `authserv_id` are removed. A third party's survive:
if this gateway sits behind an upstream that legitimately verifies mail, that
upstream's results are somebody else's to make, and a rule reading
`header.authentication-results` can still see them.

::: tip Nothing is rewritten unless something is added
The stripping filter is installed only when this gateway will add a header of
its own. With `msgauth` off, the message is spooled byte-for-byte as received.
:::

## Signing

```yaml
msgauth:
    sign:
        enabled: true
        keys:
            - domain: ngm.dev
              selector: mail
              key: /var/lib/mailgw-go/dkim/ngm.dev.mail.key
```

Publish the public half in DNS at `<selector>._domainkey.<domain>`:

```
mail._domainkey.ngm.dev.  IN TXT  "v=DKIM1; k=rsa; p=MIIBIjANBg..."
```

### Which key signs a message

The domain of its **`From:` header**, matched exactly. A key for `ngm.dev` does
not sign for `mail.ngm.dev`.

The `From` header rather than the envelope sender, because `d=` has to align
with `RFC5322.From` for DMARC to credit the signature at the far end. Signing
with the envelope sender's key is valid, costs the same bytes and buys nothing
whenever the two differ — which is every forwarded message and every bounce
this gateway generates.

Mail from a domain with no configured key goes out **unsigned**. That is the
normal case for a relay carrying other people's mail, and it is silent.

### Keys are files on this host

`key` is a **path**, never key material. A private key never travels in a
configuration bundle: the console keeps every configuration version for ever and
serves it to every gateway assigned that profile.

So a fleet cannot be given a signing key from the console — key distribution is
your problem, exactly as a real TLS certificate is. Nothing is generated for you
either: a self-generated key whose public half is not in DNS produces signatures
that **fail** verification, which is worse than not signing at all.

Put the key in the gateway's data directory (`/opt/mailgw-go/data/dkim/` in the
shipped deployment), `0600`, owned by the gateway. A key **rotated in place** is
picked up without a restart; changing *which* keys exist needs one.

Supported formats: PEM-encoded RSA (PKCS#1 or PKCS#8, at least 1024 bits per RFC
8301) and Ed25519 (RFC 8463). ECDSA is refused — it is not defined for DKIM.

### The other keys

- **`canonicalization`** — `header/body`, default `relaxed/relaxed`. A bare
  value applies to both. This is deliberately not RFC 6376's `simple/simple`
  default: `simple` breaks on any whitespace change in transit, and every hop
  between here and the recipient is entitled to make one.
- **`headers`** — which header fields are signed. Empty is the RFC 6376 §5.4.1
  recommended set, minus the ones every hop rewrites (`Received` and
  `Authentication-Results`). Must include `From`.
- **`expiration`** — the `x=` tag. `0` means the signature does not expire,
  which is the usual choice.

### Signing failures never hold mail

A key that will not load logs a warning and the message goes out **unsigned**.
Refusing to deliver because a key file lost its read permission would turn a
configuration slip into a mail outage.

Watch `mailgw_dkim_sign_failed_total` for it. Any non-zero value is a
configuration problem: mail from a domain publishing DMARC is being refused at
the far end while every log line here says delivered. Note the distinction —
*no key for this domain* is not signing, and is not counted; *there is a key and
it did not work* is failing to sign, and is.

## What is deliberately absent

- **ARC.** It matters for forwarding, which is not what this gateway does.
- **DMARC aggregate reports (`rua`/`ruf`).** A reporting pipeline is its own
  product.
- **MTA-STS, DANE, TLS-RPT.** Transport authentication, not message
  authentication.
- **A public suffix list.** DMARC's organizational-domain fallback walks up one
  label, and relaxed alignment is "equal, or one is a subdomain of the other".
  Both err towards reporting `none` or `fail` where a full implementation would
  report a pass — never the reverse. Mail from `a.b.example.com` inherits
  nothing from `example.com`'s record, and `a.example.com` does not align with
  `b.example.com`.

## Testing a rule without sending mail

`mailgw-go explain` fakes the results, on the same footing as `-tls` and
`-auth-user`. It performs no DNS.

```bash
mailgw-go explain -rcpt you@ngm.dev -dmarc fail -dmarc-policy reject
mailgw-go explain -rcpt you@ngm.dev -spf softfail
```
