# TP-08 · Claim and provisioning

**Purpose.** Take a gateway from first boot to serving mail, and verify that
every step of the path is gated where it should be.

**Managed mode only.**

**Duration.** ~30 minutes.

## Preconditions

- A running core (console, log service, database).
- A gateway. There is only one mode now — every gateway is managed, the
  development compose file has no `command:` line to delete, and file mode has
  been gone since M18. See [test environment](/environment).
- A **fresh** gateway — no data directory, or one you can delete:

```bash
docker compose down mailgw-go
sudo rm -rf /opt/mailgw-go/data/*
docker compose up -d mailgw-go
```

- An admin login on the console.

## Steps

### 1. A fresh node does not serve SMTP

```bash
swaks --server <gateway-host>:25 --quit-after CONNECT
```

**Expected.** Connection refused, or no banner. The gateway does **not** serve
mail before a configuration has been applied.

**This is deliberate.** A gateway with no allowlist would deny every peer anyway,
and a listener that can only reject looks healthy to a load balancer.

### 2. It reports not ready

```bash
curl -i http://<gateway-host>:8080/healthz
curl -i http://<gateway-host>:8080/readyz
```

**Expected.** `/healthz` returns `200` — the process is alive. `/readyz` returns
**503**, with a reason naming what is missing.

### 3. The claim code is logged and printed

```bash
docker compose logs mailgw-go | grep -i claim
```

**Expected.** A **WARN** line carrying the claim code. It is 100 bits in an
alphabet with no `I`, `L`, `O` or `U`, so it can be read aloud.

`04-gateway.sh` prints the same code at the end of its run.

**Record it.**

### 4. The wizard shows nothing without it

Open `http://<gateway-host>:8080/`.

**Expected.** One page only: the version, the node's **fingerprint**, and a field
for the claim code. No configuration form, no console URL field, nothing
actionable.

### 5. A wrong code is refused and throttled

Submit a wrong code several times quickly.

**Expected.** Each is refused. After a handful in quick succession you get
**`429`** — and the page returns immediately rather than hanging.

**It must not sleep.** A throttle that slept inside the request would let an
attacker park connections until real claims queued behind them.

### 6. The correct code signs you in

Submit it.

**Expected.** A session is established and the full wizard appears.

### 7. The code still works

Sign out — or open a second browser — and present the same code again.

**Expected.** It still works.

**The code is deliberately not consumed.** A single-use code plus a cookie leaves
the node reachable by exactly one browser for ever, and every second operator,
cleared cookie or new laptop would need a reset that signs everybody else out.
What must not reopen is the *unauthenticated* window, and that closed the moment
a code existed.

### 8. `claim status` shows it without rotating

```bash
docker compose exec mailgw-go /mailgw-go claim status -data /var/lib/mailgw-go
```

**Expected.** The same code. Your existing session still works afterwards.

### 9. `claim reset` rotates and signs everybody out

```bash
docker compose exec mailgw-go /mailgw-go claim reset -data /var/lib/mailgw-go
```

**Expected.** A **new** code, and your browser session is now invalid — reload
and you are back at the claim page. The old code no longer works.

Sign in again with the new one.

### 10. Registration lands pending

In the wizard, enter the console URL and register.

**Expected.**
- The gateway reports success.
- In the console, it appears with status **`pending`**.
- **It has not been given any configuration.**

Registration is open — anything that can reach the console may ask to join — so
the gate is approval, not registration.

### 11. A pending gateway is refused configuration

Watch the gateway's log.

**Expected.** It polls, learns it is pending, and is answered **403** for
configuration. It says so rather than retrying silently, and it still does not
serve mail.

### 12. The fingerprints match

Compare the fingerprint the console shows against the one on the gateway's own
page.

**Expected.** Identical.

**This is the step that cannot be automated away.** Approving a fingerprint you
have not compared is approving whatever registered.

### 13. Approval unblocks configuration

Approve it in the console (as an **admin** — a viewer must not be able to).

**Expected.**
- The gateway learns within seconds — it holds a WebSocket, so it should be
  near-instant rather than waiting out the 15-second poll.
- It is answered `200` for configuration.
- It still does not serve mail, because nothing has been **deployed** yet.

### 14. Assigning and deploying brings it up

Assign a `server` profile, a `ruleset`, an `allowlist` and a relay group. Press
Deploy.

**Expected.**
- The gateway pulls within a second or two.
- It logs the applied version, the rule counts and the allowlist size.
- Listeners bind.
- `/readyz` returns **200**.
- `mailgw_serving` is `1`.

### 15. It now carries mail

Run [TP-01](/plans/tp-01-smoke) against it.

### 16. `config show` reveals what it is running, redacted

```bash
docker compose exec mailgw-go /mailgw-go config show -data /var/lib/mailgw-go
```

**Expected.** The bundle, with **`[redacted]`** in place of relay passwords, the
log service API key, the metrics token and any inbound credential hashes.

**Check every one of those.** This command is in the documentation as something
to run and paste.

### 17. A revoked gateway stops being served

Revoke it in the console.

**Expected.** It is refused configuration again. Mail already queued still
delivers — revocation is a management decision, not a mail-path one.

## Cleanup

Re-approve, or wipe the data directory and start again.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 no SMTP before config | | |
| 2 not ready | | |
| 3 code logged | | |
| 4 wizard gated | | |
| 5 throttled, no sleep | | |
| 6 correct code | | |
| 7 code not consumed | | |
| 8 status does not rotate | | |
| 9 reset revokes | | |
| 10 pending | | |
| 11 403 while pending | | |
| 12 fingerprints match | | |
| 13 approval unblocks | | |
| 14 deploy brings up | | |
| 15 carries mail | | |
| 16 config show redacts | | |
| 17 revocation | | |
