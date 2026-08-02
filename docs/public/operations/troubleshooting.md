# Troubleshooting

## Nothing connects

**`550 Access denied` immediately, before any banner.** The peer is not on the
[IP allowlist](/config/allowlist). Check `ngmfilter.json`, and remember it is
fail-closed — a malformed file denies everyone.

Behind a load balancer, every connection appears to come from the balancer.
Enable [PROXY protocol](/config/server#listeners) on that listener.

**Connection refused.** The gateway is not listening. In managed mode it does not
serve SMTP at all until a configuration has been applied — check
`mailgw_serving` or `/readyz`.

**`421 4.7.0` and the connection closes.** `max.connections` is reached. Check
`mailgw_connections_throttled_total`, and look at `inactivity_timeout` before
raising the cap: a cap over sessions that never end is not a defence.

## Mail is rejected

**`451 4.3.0 No route found`.** No route rule claimed this recipient and
`default_action` applied. Ask why:

```bash
mailgw-go explain -config ./config --rcpt the@address.example --from sender@example.com
```

**A `550` you did not expect.** `explain` names the rule. If it names none, check
whether an *earlier-priority* rule matched first — rules sort by priority
ascending and first match wins.

**`552 5.3.4`** — over `max.bytes`, or the header block is over
`max.header_lines`. **`500 5.5.2`** — a line over `max.line_length`.
**`554 5.4.6`** — over `max.received_headers`; mail is looping.

All of these are permanent by design. The message can never become acceptable, so
a `4xx` would make the sender retry for four days and then get an expiry bounce
explaining nothing.

## AUTH does not work

**`AUTH` is not in the `EHLO` response.** Three things must be true: credentials
configured, and either the session encrypted or `tls.allow_insecure_auth` set.
The gateway **warns at startup** when credentials exist but AUTH can never be
advertised — check the log.

**`523 5.7.10 TLS is required`.** AUTH was attempted on a cleartext session
without `allow_insecure_auth`.

**`535 5.7.8` with what you are sure are the right credentials.** Usernames are
compared case-sensitively. Check the hash is a bcrypt hash and not a password —
though a password there would have failed the configuration load, so also check
you deployed.

**It worked, then stopped after a deploy.** Credentials are re-read per AUTH
command. If the credential set was unassigned or emptied, it stops immediately.

## Mail is queued and not moving

```bash
mailgw-go mailq -config ./config
```

The last error column says why. Common answers:

- **connection refused / timeout** — the relay is down. Fix it, then
  `mailgw-go mailq flush` rather than waiting out the backoff.
- **`535` from the relay** — outbound credentials. Check `auth_user` and whether
  `auth_pass_env` names a variable that actually exists on this host.
- **TLS errors with `tls: required`** — the relay's certificate does not verify,
  or it is below TLS 1.2.
- **blank on a first attempt** — nothing has been tried yet; check `NextAt`.

**Everything is deferring and `mailgw_delivery_connect_failed_total` is climbing
fast.** With `use_mx`, check DNS: an MX lookup failure means no destination was
reachable at all.

## Bounces are not arriving

Check `mailgw_dsn_unroutable_total`. **Any non-zero value is a configuration
problem** — notifications are being generated and dropped because no rule and no
`dsn.relay_group` gave them a relay group.

```yaml
dsn:
    enabled: true
    relay_group: Outbound
```

`check` warns about exactly this.

Also confirm the failure was a `5xx`. A `4xx` never bounces; the message is still
being retried.

## The console shows a gateway as stale

Stale means no heartbeat for three poll intervals (45 seconds). One missed poll
is normal and is not flagged.

- Can the gateway reach the console? `CORE_HOST` in the core's `.env` is the
  address **edge nodes** use — `localhost` is wrong unless single-host.
- Is the node still approved? A revoked one stops being served configuration.
- Check the gateway's own log for signature or clock errors: requests are signed
  with a ±300 second skew window, so a badly-skewed clock breaks everything.

## A deploy did not take effect

**Check `apply_error` in the console.** The gateway validates bundles itself and
keeps its last good configuration on failure; the reason is reported back.

**Check whether it needs a restart.** The allowlist, the rules and inbound
credentials hot-swap. Listeners, TLS, the relay table, spool settings and
outbound tuning do not — and the console names which ones changed.

**Compare `mailgw_config_version` across the fleet.** A node that is behind did
not pull.

To see exactly what a node is running:

```bash
mailgw-go config show -data /var/lib/mailgw-go
```

## Log rows are missing

Check `mailgw_events_spilled_total` and `mailgw_events_dropped_total`.

**Spilled** events are on disk and recoverable:

```bash
mailgw-go events              # what is parked
mailgw-go events replay       # send them now
```

**Dropped** events are gone — the in-memory buffer was full. Raise
`events.buffer_size` or `events.senders`.

Events under `failed-events/rejected/` were refused with a `4xx`: the payload
does not match the log service's schema, so resending cannot help. The file says
which.

## I have lost the claim code

```bash
mailgw-go claim status -data /var/lib/mailgw-go
```

This shows it **without rotating**, so other operators stay signed in. Use
`claim reset` only when you actually want to sign everybody out.

## I lost the gateway's data directory

The node's identity is gone. It must register again and be approved again — the
console cannot recognise it, and it holds no configuration cache.

Back up `/opt/mailgw-go/data`. Note the **spool** is a separate directory and
holds undelivered mail; a node rebuilt from a data backup alone starts with an
empty queue.
