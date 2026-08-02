# How a message flows

Understanding the stages is most of understanding the configuration: every rule
you write is evaluated at one of them, and which one decides how early the sender
finds out.

## The stages

| Stage | SMTP moment | What is known |
|---|---|---|
| `connect` | TCP accept, before the banner | peer IP and port, local IP and port |
| `helo` | after `EHLO` | the greeting name, whether the session is encrypted |
| `mail` | after `MAIL FROM` | the sender, `SIZE=`, `BODY=`, and whether the client authenticated |
| `rcpt` | after each `RCPT TO` | this recipient, its position, how many were accepted so far |
| `data` | end of `DATA` | headers, size, line count, attachments, every recipient |

A rule's stage is **inferred** from the fields it mentions: the latest stage of
any field it reads. A rule that only looks at `rcpt.domain` fires at `rcpt`. Add
one condition on `msg.size` and the same rule now fires at `data`, because that
is the first moment the answer exists.

This is why a rejection sometimes arrives on the `RCPT TO` line and sometimes at
the end of `DATA`. It is not a setting; it is a consequence of what the rule
asks about.

::: tip Why `auth.*` is a mail-stage field
Authentication happens *after* `EHLO` is answered, and connect- and helo-stage
policy has already run by then. So `auth.user` is registered at the **mail**
stage — the earliest point at which the answer is reliable.
:::

## The path

### 1. Connection

The peer's address is checked against the [IP allowlist](/config/allowlist)
before the banner is written. An address that is not on the list is answered
`550 Access denied` and closed — it never sees a greeting.

This check is **fail-closed**: a missing or malformed allowlist file denies
everyone. Starting with an empty list is refused outright unless you explicitly
set `allow_all`.

Behind an L4 load balancer, enable [PROXY protocol](/config/server#listeners) on
the listener or every connection appears to come from the balancer.

### 2. Greeting

Connect- and helo-stage policy rules run. A rejection here answers `EHLO` and
ends the session before a sender is ever named.

If [STARTTLS](/config/tls) is available it is advertised here, and if
[credentials](/config/auth) are configured, so is `AUTH`.

### 3. MAIL FROM

The [rate limits](/config/ratelimit) on the sender and the authenticated user
are checked first, before anything costs a DNS lookup or a rule walk. A refusal
here is `450` — temporary, so the sender retries.

Then [SPF](/config/msgauth) is evaluated, if anything asked for it, so a rule
can read `spf.result` here. It is the one message-authentication check
answerable this early — it never looks at the message — which is why a failing
sender can be refused on its own `MAIL` line rather than after a megabyte of
`DATA`.

Then mail-stage policy runs. The sender, the declared size, the authenticated
identity and the SPF result are all available.

### 4. RCPT TO — once per recipient

The [per-domain rate limit](/config/ratelimit) is checked first, and refuses
this recipient alone with a `450` — the rest of the transaction continues, which
is why it is enforced here rather than at `DATA`.

Then recipient-stage policy runs for this address, and the router is asked where
it should go. If no higher-priority rule needs a later stage, the route is
**decided here and cached** — and because the walk stops at the first rule that
needs `data`, an early decision is provably the one `DATA` would have reached.

A recipient rejected here is refused on its own line. The others continue.

### 5. DATA

The body is streamed to the spool as it arrives, and its facts — size, line
count, headers, hop count — are collected on the way past. Limits are enforced
here: over `max.bytes`, over `max.line_length`, too many `Received:` headers, too
many header lines. Each of those is a **permanent** refusal, because none of them
can become acceptable on a retry.

If any rule needs to see attachments, the MIME structure is walked. If anything
asked for DKIM or DMARC, the signatures are verified and alignment evaluated.
**Facts first, then rules** — so `attachment.*`, `dkim.*`, `dmarc.*` and the
`tag.attach_scan` / `tag.msgauth` verdicts are all readable by the pass that
decides what happens to the message.

Then data-stage policy runs, and any recipient whose route was not settled
earlier is routed now.

### 6. Split and enqueue

Recipients are grouped by the relay group they were routed to. Each group becomes
its own envelope with its own identity, all sharing one spooled copy of the body.
The gateway answers `250 … queued (<transaction id>)`.

Anything refused *after* the message was accepted — by a data-stage rule, say —
cannot be refused on the wire any more, so it produces a
[bounce](/operations/dsn) instead.

### 7. Delivery

The queue runner picks up envelopes as they come due, connects to the relays of
their group in priority order, and delivers.

Every header this gateway adds — `add_header` actions,
`Authentication-Results`, `Received-SPF` — is prepended *here* rather than baked
into the spooled body, which stays shared by every envelope of the transaction.
[DKIM signing](/config/msgauth#signing), when configured for the message's
`From` domain, happens after that, so the signature covers exactly the bytes
handed to the relay. Outcomes are recorded per recipient:
delivered, rejected, or deferred for another attempt.

A deferred envelope is rescheduled on the backoff list with jitter. A permanently
rejected recipient produces a bounce. An envelope that outlives `max_lifetime` is
buried and its sender told.

See [The queue](/operations/queue).

## Message identity

Every message carries a three-level identity, and the levels are literal
prefixes of each other:

```
X            the connection
X.1          the transaction (one MAIL FROM … DATA)
X.1.1        one envelope — the recipients sharing a relay group
X.1.1.1      a notification about that envelope
```

This is what makes an incident tractable: `WHERE uuid LIKE 'X%'` in the log
tables finds the connection, the transaction, every envelope it split into, and
every bounce any of them produced.
