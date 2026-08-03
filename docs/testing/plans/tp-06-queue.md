# TP-06 · Queue and retries

**Purpose.** Verify that a message survives a relay outage, that retries follow
the backoff schedule, that quarantine holds mail until a person acts, and that
`mailq` does what it says.

**Duration.** ~40 minutes, including waiting.

## Preconditions

- A provisioned stack (see [environment](../environment)). The spool lives at
  `/var/lib/mailgw-go/queue` in the container.
- A relay you can stop and start — MailHog in its own container is ideal.
- A short backoff for testing:

```yaml
outbound:
    backoff: [10s, 20s, 40s]
    max_lifetime: 5m
    delay_warning_after: 30s
```

## Steps

### 1. A message with the relay up

Send one and confirm it delivers and leaves nothing in the spool.

```bash
docker compose exec mailgw-go /mailgw-go mailq -data /var/lib/mailgw-go
```

**Expected.** All four directories empty.

### 2. With the relay down, the message is still accepted

```bash
docker compose stop mailhog
swaks --server localhost:2525 --from a@example.com --to b@ngm.dev \
      --header 'Subject: TP-06 deferral'
```

**Expected.** `250 … queued (<uuid>)`. **Record the uuid.**

**This is the durability property.** The message is on disk before the client is
told `250`; the relay being down is not the sender's problem.

### 3. It appears in the queue with a reason

```bash
docker compose exec mailgw-go /mailgw-go mailq -data /var/lib/mailgw-go
```

**Expected.** One envelope in `q/`, showing sender, recipients, attempt count,
next-attempt time, and a **last error** naming the connection failure.

::: tip A blank error on a first listing
If you look before the first attempt has run, the error column is empty — nothing
has been tried yet. Check `NextAt`.
:::

### 4. Attempts follow the backoff schedule

Watch for a couple of minutes.

**Expected.** The attempt count increments and the next-attempt time moves out
along `[10s, 20s, 40s]`, with ±15% jitter. Past the end of the list, the last
value repeats.

### 5. A delay warning is sent, once

After `delay_warning_after`:

**Expected.** A notification to the original sender saying the message is late
but still being tried — `Action: delayed`, a `4.x` status, and text saying it
will keep trying.

Check it is sent **once**, not on every subsequent attempt.

::: warning It needs somewhere to go
The warning is itself mail, and it needs a relay group. With MailHog stopped it
will queue too. Either give `dsn.relay_group` a second reachable relay, or accept
that you will see it queued rather than delivered — and verify it exists.
:::

### 6. Bringing the relay back delivers it

```bash
docker compose start mailhog
```

Wait for the next scheduled attempt.

**Expected.** Delivered, and the envelope leaves the queue.

### 7. flush skips the wait

Repeat steps 2 and 3, restart the relay, then:

```bash
docker compose exec mailgw-go /mailgw-go mailq flush -data /var/lib/mailgw-go
```

**Expected.** Every ready envelope becomes due immediately and delivers within a
poll interval, rather than waiting out the backoff.

**This is the operational move after an outage.** Without it, a recovered relay
waits up to eight hours on the shipped schedule.

### 8. Expiry buries the message and tells the sender

Stop the relay again, send a message, and wait past `max_lifetime`.

**Expected.**
- The envelope moves to `dead/`.
- Its recipients are marked expired.
- A **failure** notification goes to the sender — `Action: failed`, status
  `5.4.7`.
- `mailgw_envelopes_expired_total` increments.

```bash
docker compose exec mailgw-go /mailgw-go mailq -data /var/lib/mailgw-go
```

**Expected.** The envelope is listed under `dead/` with its final per-recipient
status and the last reply from the relay.

### 9. dead/ is metadata only

Look at the spool directory.

**Expected.** The `dead/` entry exists, and **the message body is gone** — it was
collected when the envelope was buried. There is deliberately no way to requeue
from `dead/`; the message it describes no longer exists.

### 10. A quarantine rule holds mail

```yaml
routes:
    - name: quarantine-suspicious
      priority: 50
      match: {field: mail.from_domain, op: eq, value: "suspicious.test"}
      then: [{action: quarantine}]
```

Restart, then send from `x@suspicious.test`.

**Expected.**
- The client is told **`250`** — quarantine accepts the message.
- The envelope is in `quarantine/`, not `q/`.
- `mailgw_messages_quarantined_total` increments.
- **Nothing delivers it**, however long you wait.

### 11. release puts it back

```bash
docker compose exec mailgw-go /mailgw-go mailq release <uuid> -data /var/lib/mailgw-go
```

**Expected.** The envelope moves to `q/` and delivers on the next pass.
`mailgw_envelopes_released_total` increments.

**Check the filename**, not just the listing: a released envelope must be named
in the queue's due-second format, or it would be invisible to the scheduler while
still being counted.

### 12. hold is the reverse

Queue a message with the relay down, then:

```bash
docker compose exec mailgw-go /mailgw-go mailq hold <uuid> -data /var/lib/mailgw-go
```

**Expected.** It moves from `q/` to `quarantine/` and stops being retried.

### 13. rm deletes and collects the body

```bash
docker compose exec mailgw-go /mailgw-go mailq rm <uuid> -data /var/lib/mailgw-go
```

**Expected.** The envelope is gone and its body is collected — check `data/` no
longer holds it.

### 14. An in-flight envelope cannot be touched

With a relay that accepts the connection but stalls (a `nc -l` listener that
never replies), start a delivery and then try to `rm` or `flush` that envelope.

**Expected.** Refused, saying it is in flight.

**This is not a convenience.** Deleting the file cancels nothing: the worker
finishes and writes the envelope back, so `rm` would resurrect exactly what it
deleted.

### 15. Queue depth gauges match

```bash
curl -s http://localhost:8080/metrics | grep mailgw_queue
```

**Expected.** `ready`, `inflight`, `quarantine` and `dead` match what `mailq`
lists.

### 16. A restart recovers in-flight envelopes

While an envelope is in `inflight/`, kill the gateway and restart it.

**Expected.** The envelope moves back to `q/` and is retried.

::: warning This is where duplication can happen
If the process died *after* the relay accepted the message but before the spool
was updated, the message is delivered twice. That is inherent to any spooling MTA
and is the deliberate trade — the alternative is losing it.
:::

## Cleanup

Empty the spool, restore the original `server.yaml` and `routing.yaml`, restart
the relay.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 clean delivery | | |
| 2 accepted with relay down | | |
| 3 queued with reason | | |
| 4 backoff | | |
| 5 delay warning once | | |
| 6 recovery delivers | | |
| 7 flush | | |
| 8 expiry bounces | | |
| 9 dead/ metadata only | | |
| 10 quarantine holds | | |
| 11 release | | |
| 12 hold | | |
| 13 rm | | |
| 14 in-flight refused | | |
| 15 gauges | | |
| 16 restart recovery | | |
