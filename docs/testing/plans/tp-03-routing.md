# TP-03 · Routing rules

**Purpose.** Verify that recipients are routed independently, that a rule fires
at the stage its fields imply, and that a message to several destinations is
split correctly.

**Duration.** ~30 minutes.

## Preconditions

- A provisioned stack (see [environment](../environment)).
- Two relay groups defined, both reachable — MailHog twice on different ports is
  the easiest arrangement.
- [TP-01](/plans/tp-01-smoke) passed.

Use this `routing.yaml`:

```yaml
version: 1

policy:
    - name: reject-blocked-domain
      priority: 100
      match: {field: rcpt.domain, op: eq, value: "blocked.test"}
      then:
          - {action: reject, code: 550, message: "5.7.1 not accepted"}

    - name: reject-large-from-partner
      priority: 200
      match:
          all:
              - {field: mail.from_domain, op: eq, value: "partner.test"}
              - {field: msg.size, op: gt, value: 1000}
      then:
          - {action: reject, code: 552, message: "5.3.4 too large from you"}

routes:
    - name: to-group-a
      priority: 100
      match: {field: rcpt.domain, op: eq, value: "a.test"}
      then: [{action: relay, relay: GroupA}]

    - name: to-group-b
      priority: 200
      match: {field: rcpt.domain, op: eq, value: "b.test"}
      then: [{action: relay, relay: GroupB}]

default_action: {action: tempfail, code: 451, message: "4.3.0 No route found"}
```

## Steps

### 1. check reports the inferred stages

```bash
go run ./cmd/mailgw-go check
```

**Expected.** Exit `0`, and the rule listing shows:

| Rule | Stage |
|---|---|
| `reject-blocked-domain` | **rcpt** |
| `reject-large-from-partner` | **data** |
| `to-group-a` | rcpt |
| `to-group-b` | rcpt |

The second rule is at `data` because it reads `msg.size`, even though its other
condition is a mail-stage field. **The latest field wins.**

### 2. explain agrees, without sending anything

```bash
go run ./cmd/mailgw-go explain \
    --rcpt user@a.test --from someone@example.com
```

**Expected.** `to-group-a` matched; `to-group-b` skipped; the route is `GroupA`.

### 3. A recipient-stage rejection lands on its own line

```bash
swaks --server localhost:2525 --from ok@example.com \
      --to user@blocked.test --quit-after RCPT
```

**Expected.** `550 5.7.1 not accepted` **on the `RCPT TO` line**, not after
`DATA`.

**This is the timing property.** The rule reads only `rcpt.domain`, so the sender
finds out on the line where it named the address.

### 4. One bad recipient does not sink the others

```bash
swaks --server localhost:2525 --from ok@example.com \
      --to user@blocked.test --to user@a.test \
      --header 'Subject: TP-03 mixed'
```

**Expected.**
- The first `RCPT TO` is refused `550`.
- The second is accepted `250`.
- `DATA` proceeds and the message is queued.
- It arrives in GroupA's inbox.

### 5. A message to two groups is split

```bash
swaks --server localhost:2525 --from ok@example.com \
      --to user@a.test --to other@b.test \
      --header 'Subject: TP-03 split'
```

**Expected.**
- Both recipients accepted, one `250` reply for the message.
- It arrives in **both** inboxes.
- The queue event shows **two envelopes**, `X.1.1` and `X.1.2`, sharing one
  transaction.

Check the audit trail: two Delivery rows, one per recipient, each naming the rule
that routed it in `route_rule`.

**This is what a first-recipient-wins relay gets wrong.** Both copies must go to
their own destination.

### 6. A data-stage rule fires after the body

```bash
swaks --server localhost:2525 --from bulk@partner.test --to user@a.test \
      --body "$(head -c 2000 /dev/zero | tr '\0' 'x')"
```

**Expected.** Every command through `RCPT TO` is answered `250`, and the refusal
`552 5.3.4 too large from you` comes **after the message body**, at end of
`DATA`. It could not have come earlier — the size was not known.

### 7. A small message from the same sender is accepted

```bash
swaks --server localhost:2525 --from bulk@partner.test --to user@a.test \
      --body "short"
```

**Expected.** Accepted and delivered. The rule's two conditions are ANDed.

### 8. An unrouted recipient gets the default action

```bash
swaks --server localhost:2525 --from ok@example.com --to user@nowhere.test
```

**Expected.** `RCPT TO` is answered `250`, and the refusal
`451 4.3.0 No route found` comes at **end of `DATA`**.

`default_action` applies at DATA only, so a recipient nothing routes is not
refused until the message is complete. It is a `4xx`, so the sender will retry —
which is deliberate: a `5xx` would turn a forgotten route rule into permanently
rejected mail.

### 9. Priority order decides, not file order

Add a rule with a **lower** priority number matching the same recipient as
`to-group-a`, but routing to `GroupB`. Restart or `SIGHUP`.

```bash
go run ./cmd/mailgw-go explain --rcpt user@a.test --from x@y.test
```

**Expected.** The new rule wins, because rules sort by priority **ascending** and
first match wins. Lower number = considered first.

### 10. A typo is a load-time error

Change `rcpt.domain` to `rcpt.doman` in any rule, then:

```bash
go run ./cmd/mailgw-go check
```

**Expected.** Non-zero exit, naming the unknown field, **with a suggestion** for
what you probably meant. The gateway will not start with it.

**This is the registry's whole purpose.** The alternative is a rule that silently
never fires.

## Cleanup

Restore the original `routing.yaml`, restart, drain any queued mail.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 stages inferred | | |
| 2 explain | | |
| 3 rcpt-stage timing | | |
| 4 partial rejection | | |
| 5 split | | |
| 6 data-stage timing | | |
| 7 AND semantics | | |
| 8 default action | | |
| 9 priority order | | |
| 10 typo refused | | |
