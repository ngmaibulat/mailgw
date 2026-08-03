# TP-01 · Smoke test

**Purpose.** Establish that the stack is alive and a message can travel from a
client, through the gateway, to a relay — and that the audit trail records it.

Run this first, always. It does not test any specific feature; it tells you
whether the other plans are worth starting.

**Duration.** ~10 minutes.

## Preconditions

- A running stack ([test environment](/environment)).
- MailHog reachable and its inbox **empty**.
- The gateway's allowlist includes the address you will connect from.

## Steps

### 1. The gateway answers

```bash
swaks --server localhost:2525 --quit-after CONNECT
```

**Expected.** A `220` banner naming the configured hostname.

### 2. EHLO advertises the expected extensions

```bash
swaks --server localhost:2525 --helo probe.invalid --quit-after EHLO
```

**Expected.** A `250` multi-line reply including `SIZE`, `PIPELINING`,
`8BITMIME`, `SMTPUTF8`. `DSN` is present when `dsn.enabled` is set.

Record the whole capability list — later plans compare against it.

### 3. Configuration validates

```bash
docker compose exec mailgw-go /mailgw-go check -data /var/lib/mailgw-go
# on the gateway itself:
cd mailgw-go && go run ./cmd/mailgw-go check
```

**Expected.** Exit code `0`. Output names the hostname, listeners, allowlist,
relay groups and each rule with its inferred stage.

**Record every warning.** They are not failures, but a warning you did not expect
means the configuration is not what you think it is.

### 4. A message is accepted

```bash
swaks --server localhost:2525 \
      --from sender@example.com \
      --to recipient@ngm.dev \
      --header 'Subject: TP-01 smoke test'
```

**Expected.**
- Every command answered `250`.
- The final reply contains the word **queued** and a transaction id in
  parentheses, e.g. `250 2.0.0 Message queued (A1B2C3D4-….1)`.

**Record the transaction id.** Steps 6 and 7 need it.

### 5. It arrives at the relay

Open MailHog at `http://localhost:8025`.

**Expected.** The message is present, with the subject from step 4, and its
headers include a `Received:` line naming the gateway and `X-NGM-Gateway: go`.

### 6. The queue is empty again

```bash
docker compose exec mailgw-go /mailgw-go mailq -data /var/lib/mailgw-go
```

**Expected.** No envelopes anywhere — `q/`, `inflight/`, `quarantine/` and
`dead/` are all empty. A delivered message leaves nothing behind.

::: tip If it is still in `q/`
Delivery is asynchronous; give it a few seconds. If it stays, the relay is not
accepting — check the last-error column and go to
[TP-06](/plans/tp-06-queue).
:::

### 7. The audit trail recorded it

In the console's log viewers, or directly:

```bash
curl -s 'http://localhost:3000/api/connection?q={"limit":5}' | jq
```

**Expected.** Three rows relating to the transaction id from step 4:

- a **Connection** row, uuid `X`
- a **Transaction** row, uuid `X.1`
- a **Delivery** row, uuid `X.1.1`

Each carries a `gateway` naming which node handled it; the delivery row also
carries a `route_rule` naming the rule that chose its destination.

`WHERE uuid LIKE 'X%'` finds all three — that prefix relationship is a hard
contract and this step is what checks it.

### 8. The observability endpoints answer

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
curl -s http://localhost:8080/metrics | head -30
```

**Expected.** `/healthz` returns `200`. `/readyz` returns `200`. `/metrics`
returns Prometheus text including `mailgw_messages_accepted_total` with a value
of at least `1`.

If a metrics token is configured, the first two need
`-H 'Authorization: Bearer <token>'` and answer `401` without it.

## Cleanup

Delete the message from MailHog so the next plan starts from an empty inbox.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 banner | | |
| 2 EHLO | | |
| 3 check | | |
| 4 accepted | | |
| 5 relayed | | |
| 6 queue empty | | |
| 7 audit rows | | |
| 8 endpoints | | |
