# TP-07 · Bounces and DSN

**Purpose.** Verify that a failed message tells its sender, that a bounce is
never bounced, and that the RFC 3461 parameters a sender supplies are honoured.

**Duration.** ~35 minutes.

## Preconditions

- A provisioned stack (see [environment](../environment)).
- A relay that can be made to **reject** a recipient `5xx`. A second MailHog will
  not do this — use a small `nc`-scripted listener, or a real relay with an
  address you know it refuses.
- `dsn.relay_group` set to a **reachable** group, so notifications actually
  arrive.
- [TP-06](/plans/tp-06-queue) passed.

```yaml
dsn:
    enabled: true
    return: headers
    postmaster: postmaster@devbook.local
    relay_group: Outbound
```

::: tip Most of this is automated
`tests/gw/dsn.test.ts` covers steps 3–6 and 11–18 against a scriptable relay —
which is exactly the "small nc-scripted listener" the preconditions ask for.
Run `pnpm test:e2e:gateway` first; what is left here is the routing and
unroutable-notification behaviour that needs a second relay group.
:::

## Steps

### 1. DSN is advertised

```bash
swaks --server localhost:2525 --quit-after EHLO
```

**Expected.** `250-DSN` present.

### 2. It is not advertised when notifications are off

Set `dsn.enabled: false`, restart, repeat.

**Expected.** No `DSN`. And on the wire:

```bash
printf 'EHLO probe\r\nMAIL FROM:<a@x.test>\r\nRCPT TO:<b@y.test> NOTIFY=FAILURE\r\nQUIT\r\n' \
    | nc localhost 2525
```

**Expected.** `504` for the `NOTIFY` parameter.

**This is the honesty property.** A gateway that generates no notifications must
not accept instructions about them.

Restore `dsn.enabled: true`.

### 3. A permanent rejection bounces

Send to an address your relay rejects `5xx`.

**Expected.** A notification to the original sender:

- `Content-Type: multipart/report; report-type=delivery-status`
- `Action: failed`, a `5.x` status
- `Diagnostic-Code: smtp; 550 …` quoting the relay's own reply
- `Final-Recipient: rfc822; <the address>`
- the original message's **headers** quoted (per `return: headers`)
- envelope sender `<>`

**Record the whole notification.** Steps 6–10 compare against it.

### 4. One report covers several recipients

Send one message to three addresses the relay rejects.

**Expected.** **One** notification naming all three, each with its own
per-recipient block — not three notifications.

### 5. A temporary rejection does not bounce

Make the relay answer `4xx`.

**Expected.** The envelope defers and is retried. **No notification.** It bounces
only when it eventually expires.

**Only a `5xx` bounces.** A `4xx` means "come back", and turning a transient
condition into permanent rejection is what this guards against.

### 6. A bounce is never bounced

Make the relay reject the **notification** itself `5xx`.

**Expected.**
- **No second notification** is generated.
- `mailgw_dsn_suppressed_total` increments.
- The notification leaves the queue.

::: warning It is completed, not buried
This plan used to say "buried in `dead/`". It is not: `dead/` is the
**`max_lifetime`** path, and an envelope every recipient permanently rejected is
*done* rather than given up on — `Runner` calls `Complete`, which removes it. Look
for the counter and for an empty queue, not for a `dead/` entry.
:::

**This is how two mail systems bounce at each other for ever.** The null sender
exists to stop it.

### 7. An unroutable notification is counted, loudly

Set `dsn.relay_group: ""` and remove any rule matching `mail.is_dsn`. Restart —
`check` should already warn. Trigger a bounce.

**Expected.**
- No notification is sent.
- `mailgw_dsn_unroutable_total` increments.
- An **ERROR** in the log saying the sender will not be told their mail failed.

**Any non-zero value here is a configuration problem.** Restore the setting.

### 8. Bounces can have their own route

```yaml
routes:
    - name: bounces-elsewhere
      priority: 20
      match: {field: mail.is_dsn, op: eq, value: true}
      then: [{action: relay, relay: GroupB}]
```

Restart and trigger a bounce.

**Expected.** The notification goes to GroupB while ordinary mail goes elsewhere.

### 9. The identity nests

Compare the notification's uuid with the envelope that failed.

**Expected.** A bounce for `X.1.1` is **`X.1.1.<n>`** — a literal prefix
extension, so `WHERE uuid LIKE 'X%'` still finds the whole tree.

A freshly minted root would leave a Delivery row with no Connection and no
Transaction anywhere.

### 10. Two notifications about one envelope do not collide

Arrange for a **delay warning** and then a **failure** for the same envelope —
short `delay_warning_after`, short `max_lifetime`, relay down.

**Expected.** Two distinct notifications, each with its own uuid and **its own
body**. The failure report must not have replaced the delay warning's content.

**This was a real defect.** Notification bodies used to be named for the envelope
they reported on, so the second overwrote the first — and a sender with a delay
warning still queued received the failure report twice.

## RFC 3461 parameters

### 11. NOTIFY=NEVER suppresses

```bash
swaks --server localhost:2525 --from a@example.com \
      --to reject-me@partner.test \
      --option 'rcpt-arg=NOTIFY=NEVER'
```

*(Or hand-craft: `RCPT TO:<reject-me@partner.test> NOTIFY=NEVER`.)*

**Expected.** The recipient is rejected by the relay, and **no notification** is
sent. `mailgw_dsn_notify_suppressed_total` increments.

### 12. NEVER is per recipient

One message to two rejected addresses, one with `NOTIFY=NEVER` and one without.

**Expected.** A notification naming **only** the second address. One abstaining
recipient must not silence the report for the others.

### 13. ORCPT reaches the report

```
RCPT TO:<bob@partner.test> ORCPT=rfc822;sales+q3@partner.test
```

**Expected.** The notification's per-recipient block carries

```
Original-Recipient: rfc822; sales+2Bq3@partner.test
```

**Check the encoding.** The `+` must appear as `+2B` — it is an xtext special,
and emitting it raw produces a value the receiving system decodes into something
else.

### 14. ENVID comes back verbatim

```
MAIL FROM:<a@example.com> ENVID=batch-2026-Q3
```

**Expected.** `Original-Envelope-Id: batch-2026-Q3` in the report.

And with **no** `ENVID`: the field is **absent** — not filled in with the
gateway's own identifier, which would give a sender that does use ENVID a
confident non-match.

### 15. RET overrides the configured return

With `dsn.return: headers`, send with `RET=FULL` and trigger a bounce.

**Expected.** The quoted part is `Content-Type: message/rfc822` and includes the
**body**.

Then with `RET=HDRS` against `dsn.return: full`: `text/rfc822-headers`, headers
only.

### 16. max_return_bytes still caps RET=FULL

Set `dsn.max_return_bytes: 1`, send with `RET=FULL`, trigger a bounce.

**Expected.** Headers only, `text/rfc822-headers` — the cap wins.

A per-message parameter must not let one 25 MiB message spool several copies of
itself back at its sender.

### 17. NOTIFY=SUCCESS earns a relayed report

```
RCPT TO:<b@ngm.dev> NOTIFY=SUCCESS
```

with a relay that **accepts**.

**Expected.** A notification with:

- `Action: relayed` — **not** `delivered`
- `Status: 2.0.0`
- headers only, whatever `RET` said
- wording that says the message reached the next system and this is not
  confirmation it reached the mailbox

**"relayed", not "delivered", is the honest word.** A relay accepting a recipient
says nothing about what happened afterwards.

### 18. No SUCCESS keyword, no success report

Send normally to an address that succeeds.

**Expected.** No notification at all. A success report nobody asked for is
unsolicited mail.

### 19. Malformed parameters are refused

```
RCPT TO:<b@y.test> NOTIFY=NEVER,DELAY
RCPT TO:<b@y.test> NOTIFY=MAYBE
RCPT TO:<b@y.test> ORCPT=nosuchtype
MAIL FROM:<a@x.test> RET=MAYBE
```

**Expected.** Each refused `5xx`. `NEVER` is mutually exclusive with the other
keywords.

## Cleanup

Restore `server.yaml` and `routing.yaml`; empty the spool.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 DSN advertised | | |
| 2 not advertised when off | | |
| 3 permanent bounces | | |
| 4 one report, several rcpts | | |
| 5 temporary does not | | |
| 6 never bounce a bounce | | |
| 7 unroutable counted | | |
| 8 own route | | |
| 9 identity nests | | |
| 10 two notifications distinct | | |
| 11 NOTIFY=NEVER | | |
| 12 per recipient | | |
| 13 ORCPT + xtext | | |
| 14 ENVID | | |
| 15 RET | | |
| 16 cap wins | | |
| 17 relayed report | | |
| 18 no unsolicited success | | |
| 19 malformed refused | | |
