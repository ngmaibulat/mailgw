# Rate limits

How **often** something may happen, per key.

This is the other half of the pair `max:` starts. `max.connections` bounds how
many connections exist **at once**; these bound how often anything happens at
all. A peer that opens one connection at a time and pushes a million messages
through it never trips a concurrency cap.

```yaml
ratelimit:
    connect_per_ip:       {rate: 0, per: 1m, burst: 0}
    messages_per_sender:  {rate: 0, per: 1h}
    messages_per_user:    {rate: 0, per: 1h}
    rcpts_per_domain:     {rate: 0, per: 1m}
    auth_failures_per_ip: {rate: 0, per: 10m}
    max_keys: 0
```

::: tip Nothing here can lose mail
Every refusal is **temporary** — `421` at the connection, `450` inside a
transaction, `454` for AUTH. A legitimate sender retries; a limit you set too
low costs delay, never a message. Nothing in this block can produce a 5xx.
:::

`rate: 0` disables a limit, and that is the shipped default for all five. There
is no default rate a mail gateway can pick without guessing at your traffic.

## The five limits

| Key | Checked at | Per | Answers |
|---|---|---|---|
| `connect_per_ip` | accept | connection | `421` |
| `messages_per_sender` | `MAIL FROM` | message | `450` |
| `messages_per_user` | `MAIL FROM` | message | `450` |
| `rcpts_per_domain` | `RCPT TO` | **recipient** | `450` |
| `auth_failures_per_ip` | `AUTH` | failed attempt | `454` |

**`connect_per_ip`** is the cheapest and the one that answers an abusive peer.
It is checked **inside** the allowlist, so only a peer that was allowed to
connect is ever tracked — see [Why inside the allowlist](#why-inside-the-allowlist).

**`messages_per_sender`** answers a runaway application. The address is
lower-cased, so one mailbox is one bucket however it is spelled. **The null
sender (`<>`) is never limited**: every bounce in the world shares it, and one
bucket for all of them would refuse exactly the notifications a gateway most
needs to deliver.

**`messages_per_user`** needs [inbound AUTH](/config/auth) — without a username
there is nothing to key on, and the limit is inert.

**`rcpts_per_domain`** counts **recipients**, not messages: a message to fifty
addresses at one domain costs fifty, which is the unit that matters when the
question is "how much mail is going there". It refuses that recipient alone, so
the rest of the transaction continues and the message is delivered to whoever
was accepted.

This is an *inbound* control on where mail is going. It is not
`outbound.per_group_connections`, which bounds connections to a relay.

**`auth_failures_per_ip`** is checked **before the password is compared**, so a
refusal costs no bcrypt — which is the point: a credential-stuffing run cannot
spend the CPU your delivery runner needs. Only failures spend the budget, so a
client that gets its password right every time is never affected however tight
you set it.

::: warning It refuses, it does not disconnect
The gateway answers `454` and the peer may keep trying on the same connection —
but every attempt past the limit is refused without checking anything, so it
costs nothing. What ends the connection is `inactivity_timeout`.
:::

## rate, per and burst

`rate` events per `per`, with `burst` allowed to arrive at once.

`burst: 0` means a full window's worth, which is what "100 per minute" means to
most people who write it. Set it lower to smooth traffic, or higher to tolerate
a spike on a low sustained rate:

```yaml
# 60 an hour, but up to 20 may arrive together.
messages_per_sender: {rate: 60, per: 1h, burst: 20}
```

Under the hood each key is a token bucket: it starts full, an event costs one
token, and tokens return at `rate`/`per`. There is nothing to tune beyond these
three numbers.

## max_keys

A ceiling on how many keys are tracked across all five limits at once.

A limiter keyed on remote address is itself a memory-exhaustion vector if
nothing evicts, so this is a **memory bound, not a tuning knob**. `0` means the
built-in ceiling (100,000 keys, roughly 10 MiB) — it does not mean unlimited.

Buckets that have refilled completely are dropped automatically, because a full
bucket is indistinguishable from one that never existed. If every tracked key is
genuinely active and the map is full, a new key is **allowed** rather than
refused: refusing what cannot be tracked would turn a memory ceiling into a mail
outage.

## Limits are per gateway

The counters live in memory, in one process. **Ten edge nodes with a limit of
100/min admit 1000/min between them** — divide by your fleet size when sizing a
limit.

That is deliberate. A shared counter would mean a network round trip in the
accept path, and a management-plane outage would become a mail outage — the
exact property `/readyz` is built to avoid.

They also do not survive a restart, which costs nothing: losing them re-opens a
window that was about to expire anyway.

## Changing a limit needs no restart

Rate limits are read live. Deploy a new `server.yaml` from the console, or send
`SIGHUP` in file mode, and the new numbers apply immediately — a limit you
cannot adjust without restarting a mail server during an incident is a limit you
will not use.

Two things to know about what that means:

- **The buckets survive.** Deploying an unrelated configuration change does not
  hand every peer a fresh allowance, so an attacker cannot be released by a
  routine deploy of something else.
- **Raising a limit gives faster relief, not instant relief.** Going from
  2/hour to 50/hour turns a 30-minute wait for the next event into a
  72-second one; it does not refill an already-empty bucket.

## Why inside the allowlist

`connect_per_ip` is evaluated **after** the [allowlist](/config/allowlist) and
**before** the `max.connections` cap:

```
tcp → proxy protocol → TLS → allowlist → rate limit → connection cap → SMTP
```

The cap is on the other side of the allowlist, and that is not an
inconsistency — the resource decides the side. The cap bounds one process-wide
semaphore, so a peer the allowlist is about to refuse must not be allowed to
hold a slot, or a flood from unlisted addresses would throttle real senders.

The rate limiter bounds nothing shared: every key has its own bucket and no peer
can spend another's allowance. What it costs is a **map entry per address** —
and outside the allowlist that map would be keyed by the whole internet, which
would make the limiter the memory-exhaustion problem it exists to prevent.

## Watching them

```
mailgw_ratelimited_connections_total
mailgw_ratelimited_senders_total
mailgw_ratelimited_users_total
mailgw_ratelimited_recipients_total
mailgw_ratelimited_auth_total
```

One per dimension, because a single counter would not tell you which limit to
raise. See [Observability](/operations/observability).

`mailgw-go check` prints what is in force, and says so when nothing is:

```
  ratelimit: connect_per_ip 100/1m0s, messages_per_sender 500/1h0m0s
  NOTE: rate limits are per gateway and in memory, so a fleet of N nodes admits
        N times these numbers; refusals are 4xx, never permanent
```

## What is deliberately absent

- **Greylisting.** It looks adjacent and is not: it needs *durable* triplet
  state surviving restarts, the opposite of the memory-only design here, and it
  delays legitimate mail by design.
- **Fleet-wide shared limits.** See [Limits are per gateway](#limits-are-per-gateway).
- **DNSBL / RBL lookups.** Reputation, not rate.
- **Outbound rate limiting per relay.** `outbound.concurrency` and
  `per_group_connections` bound connections; a rate limit there is a separate
  question about what receiving relays tolerate.
