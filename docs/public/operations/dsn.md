# Delivery status notifications

When a message the gateway accepted cannot be delivered, its sender is told. The
notification is an RFC 3464 `multipart/report` — the machine-readable format
every mail client understands.

```yaml
dsn:
    enabled: true
    return: headers          # headers | full
    max_return_bytes: 1048576
    postmaster: postmaster@gw1.example.com
    relay_group: Outbound
```

## When one is sent

| Trigger | When |
|---|---|
| **Permanent failure** | a relay rejected a recipient `5xx` |
| **Expiry** | the envelope outlived `max_lifetime` |
| **Delay warning** | still queued after `delay_warning_after` — sent once, not a failure |
| **Post-acceptance refusal** | a data-stage rule refused a recipient after the message was already accepted |
| **Relayed** | the sender asked with `NOTIFY=SUCCESS` |

**Only a `5xx` bounces.** A `4xx` — including the default "No route found" `451`
— means "come back", and turning a routing gap into permanent rejections is
exactly the failure this guards against.

One report covers every recipient it applies to. A message with three rejected
addresses produces one notification naming all three, which is what RFC 3464's
per-recipient section exists for.

## A bounce is never bounced

A message with a null sender, or one that is itself a notification, is buried
rather than answered. That is how two mail systems end up bouncing at each other
for ever, and RFC 5321 §4.5.5 requires the null sender on a notification
precisely to stop it.

Suppressed notifications are counted (`mailgw_dsn_suppressed_total`) — "we chose
not to" and "we forgot to" look identical from outside, and only one of them is
fine.

## Where a bounce is routed

Through the same rule engine as everything else, against a synthetic envelope
with `mail.is_dsn` set. So you can give notifications their own path:

```yaml
routes:
    - name: bounces-via-their-own-pool
      priority: 20
      match: {field: mail.is_dsn, op: eq, value: true}
      then:
          - {action: relay, relay: BouncePool}
```

Only a `relay` decision counts. A route rule that discards or rejects is a
decision about ordinary mail, and letting one swallow a notification would
silently stop senders learning their mail failed.

`dsn.relay_group` is the explicit fallback when no rule claims one. Without it,
an unroutable notification is dropped and counted
(`mailgw_dsn_unroutable_total`) — **any non-zero value there is a configuration
problem.** `check` warns when `dsn.enabled` is set and `relay_group` is not.

## What is quoted back

`return: headers` (the default) returns the message's header block;
`return: full` returns the whole thing. `max_return_bytes` caps the full form —
without it, one 25 MiB message failing across three relay groups would spool
three 25 MiB notifications.

## Honouring the sender's instructions

The gateway advertises the `DSN` extension when `dsn.enabled` is set, and honours
all four RFC 3461 parameters.

### NOTIFY

```
RCPT TO:<bob@partner.com> NOTIFY=FAILURE,DELAY
```

| Value | Effect |
|---|---|
| absent | failure notifications yes, delay warnings per your configuration |
| `NEVER` | no notification of any kind for this recipient |
| `FAILURE` | notify on permanent failure |
| `DELAY` | notify while still being retried |
| `SUCCESS` | notify when handed to the next hop |

`NEVER` is per **recipient**, so one abstaining recipient does not silence the
report for the others.

::: tip The delay default
RFC 3461 read strictly would suppress delay warnings for any message with no
`NOTIFY` at all — which is nearly all mail. The gateway takes the sender at its
word only when it said something: no `NOTIFY` keeps your configured behaviour,
and a `NOTIFY` that names its keywords and omits `DELAY` suppresses the warning.
This is the same reading Postfix applies.
:::

### ORCPT

```
RCPT TO:<bob@partner.com> ORCPT=rfc822;sales+q3@partner.com
```

Carried through to `Original-Recipient:` in any notification about that
recipient. Useful when an upstream hop rewrote the address and needs to match the
bounce back to what the sender originally wrote.

### RET

```
MAIL FROM:<alice@example.com> RET=FULL
```

`FULL` or `HDRS`, overriding `dsn.return` for this message — but never escaping
`max_return_bytes`. A success notification always returns headers only, whatever
`RET` said.

### ENVID

```
MAIL FROM:<alice@example.com> ENVID=batch-2026-Q3
```

Your own identifier for the message, returned verbatim as
`Original-Envelope-Id:` so you can match a notification to the message that
caused it.

## What is not passed on

**The gateway does not forward DSN parameters to the next hop.** Per RFC 3461
§5.2.7 that makes it the DSN boundary, and it takes responsibility for the
recipients it accepted — which it can, because it spools, retries and answers for
them itself.

Three consequences worth knowing:

- **`NOTIFY=NEVER` binds this gateway only.** Once a relay has accepted a
  recipient, a later failure produces a bounce from *that* system. The envelope
  sender is unchanged, so it still reaches your sender — who asked not to be
  told.
- **`ORCPT` is not forwarded**, so a downstream notification carries no
  `Original-Recipient:`. In practice nothing is lost here: this gateway does no
  address rewriting, so the downstream `Final-Recipient` *is* the address the
  sender used.
- **`ENVID` and `RET` are not forwarded**, so a downstream notification carries
  no `Original-Envelope-Id:` and returns whatever that system's policy chooses.

## Monitoring

| Metric | Meaning |
|---|---|
| `mailgw_dsn_generated_total` | notifications created and queued, per report |
| `mailgw_dsn_suppressed_total` | not sent — null sender, or every recipient declined |
| `mailgw_dsn_notify_suppressed_total` | recipients left out because of `NOTIFY`, per recipient |
| `mailgw_dsn_unroutable_total` | **dropped for want of a relay group. Alert on this.** |
