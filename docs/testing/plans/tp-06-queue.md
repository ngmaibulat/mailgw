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

::: tip Steps 2–8 and 10–14 are automated
`tests/gw/queue.test.ts` runs them against a relay it can break and repair on
demand, which is what makes the forty minutes of waiting here unnecessary.
Steps 17–19 — connection reuse, the pool cap and MX caching — stay manual: they
need several relays and real DNS.
:::

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
policy:
    - name: quarantine-suspicious
      priority: 50
      match: {field: header.subject, op: contains, value: "HOLDME"}
      then: [{action: quarantine}]
```

::: danger Two things here are load-bearing, and this plan used to get both wrong
It must be a **`policy`** rule, not a route. As a *route* action, `quarantine`
resolves to no relay group — and `split()` reasons that a quarantined envelope
still needs somewhere to go if it is ever released, so it **discards** instead.
Nothing reaches `quarantine/`.

And it must match a **data-stage** field. A quarantine whose fields are all known
by RCPT is decided before there is a message to hold, and is likewise a drop
("accepted and dropped" in the log). `header.*` and `msg.*` are data-stage;
`mail.from_domain` on its own is not.

A route rule must still resolve a relay group, so the envelope has somewhere to
go when it is released.
:::

Restart, then send with `HOLDME` in the subject.

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

### 17. Connection reuse is off by default

With the shipped configuration, send three messages and check the counter.

```bash
curl -s http://localhost:8080/metrics | grep -E 'connections_reused|pool_full'
```

**Expected.** Both `mailgw_delivery_connections_reused_total` and
`mailgw_delivery_pool_full_total` are `0`. Nothing is pooled unless
`outbound.reuse_connections` is deployed as `true`.

### 18. The global pool cap refuses rather than evicting

Deploy a `server` profile turning reuse on with a cap of 1, and configure **two**
relay groups pointing at different addresses (a second MailHog, or the same host
on two ports).

```yaml
outbound:
    reuse_connections: true
    max_pooled_connections: 1
    connection_idle_timeout: 5m
```

Send a message through the first relay, then one through the second.

```bash
curl -s http://localhost:8080/metrics | grep -E 'connections_reused|pool_full'
```

**Expected.** `pool_full` is at least `1` — the second relay's connection was
closed rather than kept. Nothing is deferred, nothing is bounced, and both
messages deliver: the cap costs a redial, never a delivery.

Now send a **second** message through the *first* relay.

**Expected.** `connections_reused` increases. The relay already in the pool keeps
its slot — it takes the connection out and puts the same one back — so the cap
refuses newcomers without disabling pooling for the incumbent.

::: warning Set it back
`max_pooled_connections: 1` is a test value. Restore the default (256) before
moving on; `outbound` needs a restart, so a redeploy is a gateway restart.
:::

### 19. A DNS failure is cached briefly, and does not outlive a retry

Configure a relay with `use_mx: true` and an `exchange` domain whose DNS you can
break (point the container at a resolver you can stop, or use a domain that
SERVFAILs).

Send several messages while resolution is failing.

**Expected.** Each message defers with `cannot resolve mail exchangers` in the
log and in `mailq -json`'s `LastErr` — the visible behaviour is unchanged. What
changes is that the gateway is not issuing one lookup per envelope: with
`outbound.concurrency: 10` and DNS down, lookups collapse to roughly one per
30 seconds per domain.

Restore DNS and wait for the next scheduled retry.

**Expected.** The message delivers on its next attempt. The negative cache is
shorter than the shortest `outbound.backoff` entry, so it can never be the reason
a recovered domain stays unreachable. Flushing by hand *immediately* after fixing
DNS may still report the old error for up to 30 seconds — that is the cache, and
it is expected.

## Cleanup

Empty the spool, restore the original `server.yaml` and `routing.yaml`, restart
the relay. Confirm `outbound.reuse_connections` is back off and
`max_pooled_connections` back at its default.

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
| 17 reuse off by default | | |
| 18 pool cap refuses, does not evict | | |
| 19 MX failure cached briefly | | |
