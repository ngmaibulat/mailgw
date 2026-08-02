# TP-09 · Deploy and rollback

**Purpose.** Verify that configuration deploys land, that a bad one does not take
the gateway down, that rollback restores exactly what ran before, and that
restart-required changes are reported by name.

**Managed mode only.**

**Duration.** ~30 minutes.

## Preconditions

- [TP-08](/plans/tp-08-provisioning) passed: an approved gateway carrying mail,
  which means a gateway in **managed** mode — not the development compose's
  file-mode default.
- Admin access to the console.

## Steps

### 1. Note the current state

```bash
curl -s http://<gateway-host>:8080/metrics | grep mailgw_config_version
docker compose exec mailgw-go /mailgw-go config show -data /var/lib/mailgw-go > /tmp/v1.json
```

**Record the version.**

### 2. A rule change lands fast, with no restart

Edit the assigned `ruleset` profile — add a policy rule that rejects a domain you
can test. Deploy.

**Expected.**
- The gateway applies within **a second or two** — it holds a WebSocket, so this
  should not take the full 15-second poll.
- The log records the new version and the new rule count.
- **No restart**, no dropped connections.
- The new rule takes effect: sending to that domain is refused.
- `mailgw_config_version` increments.

### 3. An allowlist change also hot-swaps

Edit the `allowlist` profile, deploy.

**Expected.** Applied without a restart; the new list applies to the next
connection.

### 4. A restart-required change is named

Edit the `server` profile — change `max.bytes`, or add a listener. Deploy.

**Expected.**
- The bundle is **applied** and recorded.
- The console shows **restart required**, and **names what changed** — `max`, or
  `listen` — rather than a bare flag.
- The gateway keeps running on the old value for those settings.

**A "restart required" with no reason is an alarm nobody can act on.** This step
is what checks the list is populated.

### 5. Restarting picks it up

```bash
docker compose restart mailgw-go
```

**Expected.** The new value is in force, the restart-required flag clears, and
the gateway comes back on the **same** version — boot follows the console's
recorded intent, not a timestamp.

### 6. An invalid ruleset is refused and mail keeps flowing

Edit the `ruleset` profile to something that will not compile — a rule matching
on `rcpt.doman`, say. Deploy.

**Expected.**
- The console records an **`apply_error`** with the compiler's own message,
  naming the unknown field.
- **The gateway keeps running the previous configuration.**
- Mail keeps flowing — verify by sending one.
- `mailgw_config_version` still shows the **old** version.

**This is the property that matters most on this page.** Validation belongs to
the thing that will run it, and a refused configuration is not in force.

### 7. A malformed allowlist is refused the same way

Deploy an `allowlist` profile whose body is not `{"allowed": [...]}`.

**Expected.** Refused, previous configuration retained, mail flowing.

::: tip The console should catch this one first
`parseProfileBody` shape-checks an allowlist profile before it is saved, because
a body that is not an allowlist would fail-close the gateway on deploy. Check
whether you were stopped at save time or at apply time, and record which.
:::

### 8. Redeploying an unchanged configuration does not churn

Press Deploy again with nothing changed.

**Expected.**
- **No new version** is created — the digest is compared and the gateway is
  simply re-pointed.
- The version number does not increment.
- The fleet does not re-pull.

**Record what the console said.** It should tell you it was unchanged rather than
silently doing nothing.

### 9. Rollback restores byte-identical configuration

Deploy a change (version N), then roll back to N−1.

**Expected.**
- The gateway applies within seconds.
- `mailgw_config_version` shows the **older** version id.
- The behaviour that was in force before the change is back.

```bash
docker compose exec mailgw-go /mailgw-go config show -data /var/lib/mailgw-go > /tmp/v1-again.json
diff /tmp/v1.json /tmp/v1-again.json
```

**Expected.** **No differences.** Rollback re-points at a stored version rather
than composing a new bundle, so nothing is re-derived and nothing can be
re-derived differently.

### 10. Rollback survives a restart

Restart the gateway.

**Expected.** It comes back on the **rolled-back** version, not the one it was on
before.

::: warning This is subtler than it looks
`applied_at` and `fetched_at` have one-second resolution, and a rollback often
lands in the same second as the deploy it undoes — so "most recent" by timestamp
would tie-break on version id, which is the very bundle the operator just
rejected.
:::

### 11. A console outage does not stop mail

```bash
docker compose stop webui        # on the core host
```

**Expected.**
- The gateway logs poll failures.
- **Mail keeps flowing** on the last good configuration.
- **`/readyz` still returns 200.**

**Readiness must not depend on reaching the console**, or one console outage
becomes a fleet-wide 503 — a management-plane failure turning into a mail
failure.

Restart the console; the gateway resumes polling.

### 12. A credential set deploys and revokes

*(If using [inbound AUTH](/plans/tp-05-auth).)* Assign a credential set, deploy,
authenticate. Then remove the user and deploy again.

**Expected.** The credential stops working on the **next AUTH command**, with no
restart — credentials are read per command, not snapshotted per session.

### 13. A viewer cannot deploy

Sign in as a `viewer` role user.

**Expected.** Approval, deploy and rollback are all refused `403`. Configuration
mutations too.

## Cleanup

Return to a known-good configuration and deploy it.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 baseline | | |
| 2 rules hot-swap | | |
| 3 allowlist hot-swaps | | |
| 4 restart named | | |
| 5 restart applies | | |
| 6 invalid ruleset refused | | |
| 7 malformed allowlist | | |
| 8 unchanged does not churn | | |
| 9 rollback byte-identical | | |
| 10 survives restart | | |
| 11 console outage | | |
| 12 credential revocation | | |
| 13 viewer refused | | |
