# Observability

Three endpoints and a stream of audit events.

## Endpoints

The gateway's admin listener serves:

| Path | Purpose | Auth |
|---|---|---|
| `/healthz` | liveness — the process is up | always open |
| `/readyz` | readiness — it can actually serve mail | bearer token, if configured |
| `/metrics` | Prometheus text format | bearer token, if configured |

```yaml
# admin.json, or the console's GATEWAY_METRICS_TOKEN
{ "metrics_token": "…" }
```

Empty means open, which is what every installation that firewalled the admin port
already has. `/healthz` is always open, because a liveness probe that can fail
on a credential is worse than no liveness probe.

::: tip /readyz performs no I/O
It reads the state the last successful poll left behind rather than asking the
console anything. Readiness that depended on reaching the console would turn one
console outage into a fleet-wide 503 — a management-plane failure becoming a mail
failure, which is precisely what it exists to avoid.

Ready means: provisioned, approved, a configuration applied, and serving. In file
mode, just serving. A failed poll, a failed deploy of a *new* version, and a
pending restart are all deliberately **not** conditions.
:::

## Counters

Names are stable; treat them as an interface.

### Connections

```
mailgw_connections_accepted_total     past the IP allowlist
mailgw_connections_denied_total       refused by the allowlist, before the banner
mailgw_connections_throttled_total    refused 421 — max.connections reached
mailgw_proxy_dropped_total            bad or untrusted PROXY header
```

`throttled` is a **subset** of `accepted`: the cap sits outside the allowlist, so
the peer had already been allowed.

### Authentication

```
mailgw_auth_succeeded_total
mailgw_auth_failed_total
```

Per AUTH **command**. Alert on the rate of failures.

### Rate limits

```
mailgw_ratelimited_connections_total
mailgw_ratelimited_senders_total
mailgw_ratelimited_users_total
mailgw_ratelimited_recipients_total
mailgw_ratelimited_auth_total
```

One per dimension, because a single counter would not tell you **which** limit
to raise.

**Three units.** Connections are per connection, senders and users are per
message, recipients are per **recipient** (so one message to fifty limited
addresses contributes fifty), and auth is per AUTH command.

`mailgw_ratelimited_connections_total` is a **subset** of
`mailgw_connections_accepted_total`, like `mailgw_connections_throttled_total`
beside it — this limiter sits inside the allowlist, so the peer had already been
allowed.

`mailgw_ratelimited_auth_total` is **not** a subset of
`mailgw_auth_failed_total`: a refusal there happens before any password is
compared, so it is not counted as a failed authentication. The two together are
what a credential-stuffing run looks like.

All five are zero unless [rate limits](/config/ratelimit) are configured, and
every one of them counts a **4xx** — nothing in the rate limiter is permanent.

### Message authentication

```
mailgw_spf_checked_total
mailgw_spf_failed_total
mailgw_dkim_verified_total
mailgw_dkim_signed_total
mailgw_dkim_sign_failed_total
```

**Two units here.** The first three are per inbound **message**;
`mailgw_dkim_verified_total` counts a message, not a signature, so one message
carrying three signatures counts once. The last two are per outbound
**envelope-attempt** — one message split across three relay groups and retried
twice signs six times, because the signature is computed at delivery over the
headers this gateway prepends.

`mailgw_spf_failed_total` is a **subset** of `mailgw_spf_checked_total`, not a
sibling. Nothing is refused on it by default.

All five are zero unless [message authentication](/config/msgauth) is on — which
a rule reading `spf.*`, `dkim.*` or `dmarc.*` does by itself.

There is deliberately **no DMARC counter**: a DMARC result is a function of the
SPF and DKIM results already here.

### Messages

```
mailgw_messages_accepted_total        answered 250 at end of DATA
mailgw_messages_rejected_total        refused 5xx
mailgw_messages_tempfailed_total      refused 4xx
mailgw_messages_discarded_total       accepted, sent nowhere
mailgw_messages_quarantined_total     held back
mailgw_messages_loop_rejected_total   over the hop limit
```

**`accepted` is a superset of `discarded` and `quarantined`**, not a disjoint
bucket — a message every rule dropped was still answered `250`.

### Recipients

```
mailgw_recipients_accepted_total
mailgw_recipients_rejected_total
mailgw_recipients_tempfailed_total
mailgw_recipients_discarded_total
```

### Delivery

```
mailgw_delivery_attempts_total            per ENVELOPE-attempt
mailgw_delivery_delivered_total           per RECIPIENT
mailgw_delivery_bounced_total             per RECIPIENT, rejected 5xx
mailgw_delivery_deferred_total            per ENVELOPE-attempt
mailgw_delivery_connect_failed_total      per RELAY
mailgw_delivery_connections_reused_total  per RELAY-attempt
mailgw_delivery_pool_full_total           per RELAY-attempt
mailgw_delivery_tls_downgraded_total      per RELAY-attempt
```

::: warning The units differ within this group, and the difference is real
`connect_failed` is **per relay** — one attempt over a wholly unreachable
three-member group adds three, which is what makes it useful for spotting one bad
relay among several. `deferred` is **per envelope-attempt** — one, however many
relays were tried.
:::

The last three are `0` unless `outbound.reuse_connections` is on.
`pool_full` counts finished connections closed rather than kept, because
`outbound.max_pooled_connections` was already reached — **nothing fails when it
moves**; the next message to that relay dials again. Being over
`per_group_connections` is not counted here: that is one relay behaving as
configured, where this is the global ceiling.

### Notifications

```
mailgw_dsn_generated_total
mailgw_dsn_suppressed_total
mailgw_dsn_notify_suppressed_total
mailgw_dsn_unroutable_total
```

### Queue depth (gauges)

```
mailgw_queue_ready
mailgw_queue_inflight
mailgw_queue_quarantine
mailgw_queue_dead
mailgw_failed_events
mailgw_failed_events_rejected
```

These are **omitted rather than reported as zero** when the spool cannot be read
— a managed gateway before its first configuration has no spool, and a fabricated
`0` would read as "drained" when it means "unreadable".

### Audit pipeline

```
mailgw_events_spilled_total         parked on disk, replayable
mailgw_events_replayed_total        a replay delivered them
mailgw_events_replay_failed_total   given up on permanently
mailgw_events_dropped_total         GONE — never written anywhere
```

`dropped` and `spilled` are not the same severity. A spilled event is on disk and
a replay can still deliver it; a dropped one was lost because the in-memory
buffer was full. Non-zero `dropped` means `events.buffer_size` or
`events.senders` cannot keep up.

### State

```
mailgw_build_info{version,commit}
mailgw_serving        1 when listeners are bound
mailgw_managed        1 when centrally managed
mailgw_approved       1 when the console has approved this node
mailgw_config_version the applied configuration version
```

## Worth alerting on

| Signal | Why |
|---|---|
| `mailgw_dsn_unroutable_total > 0` | senders are not learning their mail failed |
| `rate(mailgw_events_dropped_total)` | audit rows are being lost unrecoverably |
| `mailgw_queue_ready` climbing | a relay stopped accepting |
| `rate(mailgw_auth_failed_total)` | credential stuffing |
| `rate(mailgw_ratelimited_senders_total)` sustained | usually one runaway application — find which sender before raising the limit |
| `rate(mailgw_ratelimited_connections_total)` sustained | either an abusive peer or a limit set below your real traffic |
| `mailgw_dkim_sign_failed_total > 0` | mail is going out unsigned and being refused at the far end while the logs here say delivered |
| `mailgw_delivery_tls_downgraded_total` rising | a relay that used to encrypt stopped |
| `rate(mailgw_delivery_pool_full_total)` sustained | connection reuse is on but the global cap, not your workload, is deciding what gets pooled — raise `outbound.max_pooled_connections` |
| `mailgw_messages_loop_rejected_total > 0` | mail is looping |
| `mailgw_config_version` differing across the fleet | a deploy did not land |

## Audit events

Every connection, transaction and delivery is posted as JSON to a logging
service, which stores them for the console's log viewers. Each row carries the
`gateway` that wrote it, and a delivery row also carries the `route_rule` that
sent that recipient there.

Posting is **asynchronous and never blocks mail**. If the service is unreachable,
events are retried, then parked in `failed-events/` and replayed later:

```bash
mailgw-go events                  # what is parked
mailgw-go events replay           # send it now
mailgw-go events -all             # include what was given up on
```

A `4xx` from the logging service is terminal — the payload does not match its
schema, so resending cannot help — and the event is filed under
`failed-events/rejected/` rather than retried for ever. Those files are the
evidence of what was refused; they are swept after
`events.rejected_retention` (30 days by default).
