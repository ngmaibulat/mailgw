# TP-10 · Observability

**Purpose.** Verify that the endpoints answer correctly, that the counters mean
what their HELP strings say, and that the audit trail survives the log service
going away.

**Duration.** ~30 minutes.

## Preconditions

- A running stack.
- [TP-01](/plans/tp-01-smoke) passed.
- A way to stop and start the log service.

## Endpoints

### 1. healthz is liveness only

```bash
curl -i http://<gateway-host>:8080/healthz
```

**Expected.** `200`, and it answers **even before a configuration is applied**
and **even with a metrics token configured**. A liveness probe that can fail on a
credential is worse than no liveness probe.

### 2. readyz reflects real readiness

```bash
curl -i http://<gateway-host>:8080/readyz
```

**Expected.** `200` on a provisioned, approved, configured, serving gateway.
`503` with a **reason** otherwise — see [TP-08](/plans/tp-08-provisioning) step 2.

### 3. readyz performs no I/O

Stop the console. Wait past a poll interval.

**Expected.** `/readyz` **still returns 200**, and answers immediately — it reads
the state the last successful poll left behind rather than asking anything.

**A failed poll is deliberately not a readiness condition.** Nor is a failed
deploy of a *new* version, nor a pending restart.

### 4. The metrics token gates the right endpoints

Configure `admin.metrics_token`. Restart.

```bash
curl -i http://<gateway-host>:8080/metrics       # expect 401
curl -i http://<gateway-host>:8080/readyz        # expect 401
curl -i http://<gateway-host>:8080/healthz       # expect 200
curl -i -H 'Authorization: Bearer <token>' http://<gateway-host>:8080/metrics
```

**Expected.** As annotated. `/healthz` stays open.

### 5. An empty token means open

Remove it, restart.

**Expected.** All three answer without a credential — which is what every
installation that firewalled the port already has. Check the log says the token
was removed.

## Counters

### 6. Connection counters, and the subset relationship

Make one accepted connection and one denied by the allowlist.

```bash
curl -s http://<gateway-host>:8080/metrics | grep mailgw_connections
```

**Expected.** `accepted` and `denied` each incremented once.

Now set `max.connections: 1` and open two connections at once.

**Expected.** The second is answered `421 4.7.0` and closed, and
`mailgw_connections_throttled_total` increments — **and so does `accepted`.**

**`throttled` is a subset of `accepted`, not a sibling.** The cap sits outside
the allowlist, so the peer had already been allowed. If throttled incremented
without accepted, the ordering is wrong.

### 7. Message counters, and the superset relationship

Send: one delivered message, one refused by a policy rule, one **discarded** by a
rule.

**Expected.**
- `mailgw_messages_accepted_total` = **2** — the delivered one and the discarded
  one.
- `mailgw_messages_rejected_total` = 1.
- `mailgw_messages_discarded_total` = 1.

**`accepted` is a superset of `discarded`.** A message every rule dropped was
still answered `250`, and counting it only as discarded would make the accepted
figure a lie.

### 8. Delivery counters have different units, deliberately

Configure a relay group with **three** members, all unreachable. Send one message.

**Expected.**
- `mailgw_delivery_connect_failed_total` increments by **3** — per relay.
- `mailgw_delivery_deferred_total` increments by **1** — per envelope-attempt.

**Read the HELP strings and check they say so.** This mixed-unit group is the
easiest thing on this page to misread in a dashboard.

### 9. Recipient counters are per recipient

Send one message to three recipients, all accepted.

**Expected.** `mailgw_recipients_accepted_total` increments by 3,
`mailgw_messages_accepted_total` by 1.

### 10. Queue gauges match reality

With the relay down, queue two messages and quarantine one.

```bash
curl -s http://<gateway-host>:8080/metrics | grep mailgw_queue
docker compose exec mailgw-go /mailgw-go mailq -data /var/lib/mailgw-go
```

**Expected.** The gauges match the listing exactly.

### 11. Gauges are omitted, not zeroed, when unreadable

On a **fresh** managed gateway before its first configuration:

**Expected.** The `mailgw_queue_*` gauges are **absent** from `/metrics` — not
present with value `0`.

**A fabricated zero reads as "drained" when it means "unreadable".** A dashboard
cannot tell the difference; an absent series can.

### 12. Build and state metrics

```bash
curl -s http://<gateway-host>:8080/metrics | grep -E 'mailgw_(build_info|serving|managed|approved|config_version)'
```

**Expected.** `build_info` carries version and commit labels. `serving` is 1.
`managed` and `approved` are 1 on a managed node. `config_version` matches the
console.

## The audit trail

### 13. Three rows per message

Covered in [TP-01](/plans/tp-01-smoke) step 7. Re-verify the prefix relationship:
Connection `X`, Transaction `X.1`, Delivery `X.1.1`.

### 14. A connection that never sends still produces a row

```bash
swaks --server localhost:2525 --quit-after EHLO
```

**Expected.** A **Connection** row, with no Transaction and no Delivery.

Connections that never reach DATA used to leave nothing at all — which meant a
connect- or helo-stage reject rule caught things that appeared nowhere.

### 15. A STARTTLS connection produces exactly one

Send one message over STARTTLS.

**Expected.** **One** Connection row, not two, despite the session being
discarded and recreated by the upgrade.

### 16. Rows carry gateway and route_rule

**Expected.** Every row has a `gateway`; each Delivery row also has a
`route_rule` naming the rule that chose that recipient's destination.

Send one message to two recipients routed by **different** rules.

**Expected.** Two Delivery rows with **different** `route_rule` values —
it is per recipient, not per envelope.

### 17. Mail keeps flowing when the log service is down

```bash
docker compose stop logservice
```

Send several messages.

**Expected.**
- **Every message is accepted and delivered normally.** Mail must never wait on
  the audit trail.
- `mailgw_events_spilled_total` increments.
- Files appear under `failed-events/`.

### 18. Spilled events replay

```bash
docker compose start logservice
docker compose exec mailgw-go /mailgw-go events -data /var/lib/mailgw-go
docker compose exec mailgw-go /mailgw-go events replay -data /var/lib/mailgw-go
```

**Expected.** The parked events are listed, then delivered.
`mailgw_events_replayed_total` increments and the directory drains. The rows
appear in the log tables.

### 19. A rejected event is not retried for ever

Cause the log service to answer `4xx` — a schema mismatch. Run a replay.

**Expected.** The event moves to `failed-events/rejected/`,
`mailgw_events_replay_failed_total` increments, and later replays **do not** keep
trying it. The file says why.

```bash
docker compose exec mailgw-go /mailgw-go events -all -data /var/lib/mailgw-go
```

**Expected.** `-all` lists them; without it they are hidden.

### 20. Dropped is worse than spilled

Set `events.buffer_size` very low and send a burst.

**Expected.** `mailgw_events_dropped_total` increments and **nothing appears on
disk** for those events.

**These two are not the same severity.** A spilled event is parked and
replayable; a dropped one was never written anywhere and is gone. Confirm the
HELP strings say so.

## Cleanup

Restore configuration, restart both services, drain the queue.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 healthz always open | | |
| 2 readyz | | |
| 3 no I/O | | |
| 4 token gates | | |
| 5 empty = open | | |
| 6 throttled ⊂ accepted | | |
| 7 accepted ⊃ discarded | | |
| 8 delivery units | | |
| 9 per recipient | | |
| 10 gauges match | | |
| 11 omitted not zeroed | | |
| 12 build/state | | |
| 13 three rows | | |
| 14 connection-only row | | |
| 15 STARTTLS once | | |
| 16 gateway + route_rule | | |
| 17 mail flows without logs | | |
| 18 replay | | |
| 19 rejected terminal | | |
| 20 dropped vs spilled | | |
