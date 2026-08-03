# server.yaml

Everything about the process itself. Every key has a default, and a `server.yaml`
that does not exist is the same as one that sets nothing.

## Identity

```yaml
hostname: gw1.example.com
greeting: ""                     # extra text on the banner
local_domains: [example.com, internal.example]
```

`hostname` is the SMTP banner name and the name in the `Received:` header this
gateway adds. It is also what labels this gateway's rows in the log tables when
it is not centrally managed.

## Listeners

```yaml
listen:
    - addr: "0.0.0.0:25"
    - addr: "0.0.0.0:465"
      implicit_tls: true
    - addr: "0.0.0.0:2525"
      proxy_protocol: true
      proxy_trusted: ["10.0.0.0/8"]
```

| Key | Meaning |
|---|---|
| `addr` | address and port to bind |
| `implicit_tls` | wrap the socket in TLS from the first byte (submissions, port 465). Needs a keypair. |
| `proxy_protocol` | read a PROXY header (v1 or v2) from every connection |
| `proxy_trusted` | CIDRs allowed to send one. **Required** when `proxy_protocol` is set |

### PROXY protocol

Behind an L4 load balancer every connection arrives from the balancer's address,
so allowlisting it opens the relay to everything behind it. PROXY protocol is how
the gateway sees the real client.

It is **fail-closed**: no valid header, or a peer outside `proxy_trusted`, and the
connection is dropped without a reply. A PROXY header is trivially forged, which
is why `proxy_trusted` is required and must be non-empty — it is the only thing
between a forged header and an open relay. **A listener with this set must not be
reachable directly.**

## Limits

```yaml
max:
    bytes: 26214400          # 25 MiB
    line_length: 512
    recipients: 100
    received_headers: 100
    header_lines: 1000
    connections: 1024
```

| Key | Over it | Reply |
|---|---|---|
| `bytes` | message too large | `552 5.3.4` |
| `line_length` | a line too long | `500 5.5.2` |
| `recipients` | too many `RCPT TO` | go-smtp's own refusal |
| `received_headers` | RFC 5321 §6.3 hop limit — mail is looping | `554 5.4.6` |
| `header_lines` | header block too wide | `552 5.3.4` |
| `connections` | too many concurrent sessions | `421 4.7.0`, then close |

The first four are **permanent** refusals. None of those conditions can become
acceptable on a retry, so a `4xx` would only make the sender try for four days
and then receive an expiry bounce that explained nothing.

`connections` is a process-wide cap across every listener, because what it bounds
is this process's file descriptors. It is worth nothing without
`inactivity_timeout` — a cap over sessions that never end is a denial-of-service
primitive, not a defence. **Must be positive**; raise it rather than writing `0`.

## Timeouts

```yaml
inactivity_timeout: 300s
shutdown_timeout: 10s
```

`inactivity_timeout` is the per-socket read and write deadline. An explicit `0`
is rejected, because it means an unbounded slowloris.

`shutdown_timeout` bounds the **whole** teardown — draining sessions, the
delivery attempt in flight, and the audit events behind it — not any one step.
Your container's stop grace period must exceed it, or the runtime kills the
process part-way through and the careful ordering buys nothing. Both shipped
compose files set `stop_grace_period: 30s`.

## Protocol

```yaml
smtputf8: true
```

## Logging

```yaml
log:
    level: info      # debug | info | warn | error
    format: json     # json | text
```

## Outbound

```yaml
outbound:
    concurrency: 10
    per_group_connections: 5
    poll_interval: 5s
    spool_dir: /opt/mailgw-go/queue
    backoff: [60s, 5m, 15m, 30m, 1h, 2h, 4h, 8h]
    max_lifetime: 96h
    delay_warning_after: 4h
    jitter: 0.15
    connect_timeout: 30s
    data_timeout: 10m
    reuse_connections: false
    connection_idle_timeout: 30s
    max_pooled_connections: 256
    mx_cache_ttl: 5m
```

`poll_interval` is a **ceiling on how long the scheduler sleeps, not a fixed
tick**. Queued work wakes it exactly when due, and new mail wakes it immediately.
All it bounds is how long an envelope dropped into the queue directory by hand
goes unnoticed — so it is cheap to raise.

`backoff` is walked per attempt; past the end of the list the last value repeats.
`jitter` spreads retries so a relay coming back does not meet a thundering herd.

`reuse_connections` ships **off**. Turning it on changes what every relay in the
field sees — per-connection message caps, connection-keyed rate limits — and
nothing observable in a default deployment shows a need for it. The three keys
below it are read only when it is on, but they are defaulted anyway, so enabling
reuse is a one-line change rather than four.

`max_pooled_connections` bounds idle connections across **every** relay.
`per_group_connections` bounds one, and a pooled connection is identified by the
address it dials — so with [`use_mx`](/config/relays#use-mx) the set grows with
whatever DNS names, not with your relay table. Without a global number the real
ceiling is `per_group_connections × distinct mail exchangers seen within
connection_idle_timeout`, which nothing states.

At the cap the gateway **closes the connection it just finished with rather than
evicting somebody else's**. Nothing fails — the next message to that relay dials
again — and a relay already in the pool is unaffected, because taking a
connection out frees its slot and putting it back reclaims it. Watch
`mailgw_delivery_pool_full_total`; a steady rate means the cap rather than the
workload is deciding what gets pooled, and the answer is to raise the number. It
must be positive when `reuse_connections` is on: set it high to lift the ceiling,
rather than to `0`.

`mx_cache_ttl` is how long an MX answer is reused. DNS **failures** are cached
too, but only for 30 seconds and not configurably — see
[`use_mx`](/config/relays#use-mx).

See [The queue](/operations/queue).

## Bounces

```yaml
dsn:
    enabled: true
    return: headers          # headers | full
    max_return_bytes: 1048576
    postmaster: postmaster@gw1.example.com
    relay_group: Outbound
```

`relay_group` is the fallback for notifications no route rule claims. Without it,
a bounce that no rule matches is dropped and the sender never learns their mail
failed — `check` warns about exactly this.

See [Delivery status notifications](/operations/dsn).

## Attachment scanning

```yaml
attach:
    enabled: false
    url: http://logservice:3000/filter/md5
    timeout: 3s
    fail: closed             # closed | open
    include_inline: true
    on_block: reject         # reject | tempfail | quarantine | discard
```

Ships **off**. It needs a reachable blocklist endpoint and rows in it to do
anything, and turning it on changes what every message costs — the MIME walk is
a second pass over the spooled body.

`fail: closed` means a scanner outage answers `451`, not `550`: an outage is
temporary and is not the sender's doing.

## Message authentication

```yaml
msgauth:
    spf: {enabled: false}
    dkim: {enabled: false}
    dmarc: {enabled: false}
    authserv_id: ""          # empty means `hostname`
    max_dkim_signatures: 10
    dns_timeout: 5s
    sign:
        enabled: false
        keys: []             # domain + selector + a PATH on this host
        canonicalization: relaxed/relaxed
        headers: []          # empty is the RFC 6376 set; must include From
        expiration: 0
```

Ships **off**, like `attach` above and for the same reason. A rule reading
`spf.*`, `dkim.*` or `dmarc.*` turns the matching check on by itself, so these
flags are for wanting the `Authentication-Results` header without a rule.

Full detail in [Message authentication](/config/msgauth).

## Rate limits

```yaml
ratelimit:
    connect_per_ip:       {rate: 0, per: 1m, burst: 0}
    messages_per_sender:  {rate: 0, per: 1h}
    messages_per_user:    {rate: 0, per: 1h}
    rcpts_per_domain:     {rate: 0, per: 1m}
    auth_failures_per_ip: {rate: 0, per: 10m}
    max_keys: 0
```

How **often** things may happen, where `max:` above is how many at once. All
off; every refusal is a 4xx, so a limit set too low costs delay rather than
mail. Read live — changing one needs no restart.

Full detail in [Rate limits](/config/ratelimit).

## Audit events

```yaml
events:
    timeout: 3s
    retries: 3
    buffer_size: 4096
    senders: 4
    api_key_env: API_KEY
    replay_interval: 5m
    rejected_retention: 720h
```

`replay_interval: 0` disables the background replay pass — and with it the
retention sweep that rides on it, which the gateway warns about at boot.

## TLS

See [TLS](/config/tls).
