# Actions

A rule's `then` is a list of actions. Most of them **end** evaluation for
whatever they apply to; two accumulate and let evaluation continue.

```yaml
then:
    - {action: add_header, name: "X-Scanned", value: "yes"}   # accumulates
    - {action: relay, relay: Outbound}                        # terminal
```

## Terminal actions

### relay

```yaml
- {action: relay, relay: Outbound}
```

Send this recipient to the named [relay group](/config/relays). A group that does
not exist is a configuration error at load time, not a delivery failure later.

### reject

```yaml
- {action: reject, code: 550, message: "5.7.1 not accepted here"}
```

Refuse permanently. The sender is told and will not retry.

The code must be `5xx`. Where it lands depends on the rule's stage: at `rcpt` it
refuses that one address on its own line; at `data` it refuses the whole message;
at `connect` or `helo` it ends the session before a sender is named.

### tempfail

```yaml
- {action: tempfail, code: 451, message: "4.3.2 try again shortly"}
```

Refuse temporarily. The sender is expected to retry. Use this for conditions that
will pass — a downstream system in maintenance, a rate you are shedding — and
never for a message that can never be accepted.

::: tip Tempfail never bounces
Only a `5xx` produces a [bounce](/operations/dsn). A `4xx` means "come back", and
turning a routing-configuration gap into permanent rejections is the failure mode
that design guards against.
:::

### discard

```yaml
- {action: discard}
```

Accept the message and send it nowhere. The client is told `250`. Silent by
definition, so it is counted (`mailgw_messages_discarded_total`,
`mailgw_recipients_discarded_total`) — a discard nobody can see is
indistinguishable from mail loss.

### quarantine

```yaml
- {action: quarantine}
```

Accept the message and hold it in the spool's quarantine directory instead of
queueing it. The client is told `250`.

**Nothing drains quarantine on its own.** Release it deliberately:

```bash
mailgw-go mailq                                # see what is held
mailgw-go mailq release <uuid> -config ./config
```

### accept

```yaml
- {action: accept}
```

Stop policy evaluation and accept — a whitelist that stops later rules refusing
this message. It does **not** stop routing: the routes list is evaluated
separately and still decides where the mail goes.

Scope follows the stage: an `accept` at `connect` or `helo` covers the whole
connection, one at `mail` or `data` covers that message only.

## Accumulating actions

### add_header

```yaml
- {action: add_header, name: "X-Routed-By", value: "mailgw"}
```

Prepend a header to the delivered copy. Headers accumulate across every rule that
matches, and land above the message's own — the body itself is never modified, so
one spooled copy can still be shared by every envelope the message split into.

Scope follows the stage, as with `accept`: connect- and helo-stage headers apply
to every message on the connection.

### tag {#tag}

```yaml
- {action: tag, key: "scanned", value: "clean"}
```

Set a value a later rule can read as `tag.scanned`. Tags are how one rule passes
a conclusion to another without either of them knowing about the other:

```yaml
policy:
    - name: mark-external
      priority: 10
      match: {not: {field: auth.authenticated, op: eq, value: true}}
      then:
          - {action: tag, key: "origin", value: "external"}

    - name: external-cannot-mail-payroll
      priority: 20
      match:
          all:
              - {field: "tag.origin", op: eq, value: "external"}
              - {field: rcpt.domain, op: eq, value: "payroll.internal"}
      then:
          - {action: reject, code: 550, message: "5.7.1 internal recipients only"}
```

Tags set while evaluating one recipient do not leak into the next. Tags set at
`RCPT` are carried forward into the data-stage pass for that recipient.

## Where each action is legal

`discard` and `quarantine` are refused at compile time **before** `MAIL FROM` —
at `connect` or `helo` there is no message yet to drop. Everything else is legal
at any stage; what changes is where the reply lands.

## default_action

```yaml
default_action:
    action: tempfail
    code: 451
    message: "4.3.0 No route found"
```

What happens to a recipient no route rule claimed. It applies **at `DATA` only**,
so a recipient with no route is not refused until the message is complete.

`tempfail` is the shipped default and the right one: a `5xx` here would turn "I
forgot to write a route rule" into permanently rejected mail.
