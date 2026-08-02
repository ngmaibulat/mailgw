# TP-04 · Inbound TLS

**Purpose.** Verify STARTTLS on a plain port, implicit TLS on a dedicated one,
that policy is re-evaluated after an upgrade, and that a renewed certificate is
picked up without a restart.

**Duration.** ~25 minutes.

## Preconditions

- File mode.
- A certificate and key the gateway can read. A self-signed pair is fine:

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -keyout /tmp/tp-tls/server.key -out /tmp/tp-tls/server.crt \
    -subj "/CN=devbook.local"
```

- `openssl s_client` available.

## Steps

### 1. No keypair means no STARTTLS

```yaml
tls:
    cert: ""
    key: ""
    starttls: true
```

Restart, then:

```bash
swaks --server localhost:2525 --quit-after EHLO
```

**Expected.** No `STARTTLS` in the capability list, despite `starttls: true`. The
setting is **inert without a keypair** — a configuration that mentions TLS but
has no certificate behaves exactly as one that does not.

### 2. A keypair enables it

```yaml
tls:
    cert: /tmp/tp-tls/server.crt
    key: /tmp/tp-tls/server.key
    starttls: true
```

Restart, then repeat step 1's command.

**Expected.** `250-STARTTLS` present.

### 3. The upgrade works

```bash
swaks --server localhost:2525 --tls --from a@example.com --to b@ngm.dev
```

**Expected.** `STARTTLS` accepted `220`, handshake completes, the message is
delivered. `swaks` reports the negotiated version and cipher — record them; TLS
1.2 is the floor.

### 4. starttls: false opts out

```yaml
tls:
    cert: /tmp/tp-tls/server.crt
    key: /tmp/tp-tls/server.key
    starttls: false
```

Restart, then EHLO.

**Expected.** No `STARTTLS`, even though a keypair is configured.

### 5. Implicit TLS on its own listener

```yaml
listen:
    - addr: "0.0.0.0:2525"
    - addr: "0.0.0.0:4465"
      implicit_tls: true
```

Restart, then:

```bash
openssl s_client -connect localhost:4465 -quiet
```

**Expected.** The TLS handshake completes **before** any SMTP, and the `220`
banner arrives inside the encrypted session.

```bash
swaks --server localhost:4465 --tlsc --from a@example.com --to b@ngm.dev
```

**Expected.** Delivered.

### 6. The allowlist refusal is readable on the TLS listener

Remove your address from `ngmfilter.json` and restart, then connect to the
implicit-TLS port again.

**Expected.** The TLS handshake **completes**, and the `550 Access denied` is
readable **inside** the TLS session.

**This is the ordering property.** If the refusal were written before the
handshake, the client would see a protocol error rather than a reason — the
allowlist has to sit outside the TLS wrapper, not inside it.

Restore your address afterwards.

### 7. Rules see the encrypted session

Add:

```yaml
policy:
    - name: require-tls
      priority: 100
      match: {field: conn.tls, op: eq, value: false}
      then:
          - {action: reject, code: 530, message: "5.7.0 encryption required"}
```

Restart.

**Expected.**
- A plaintext session is refused `530` at `EHLO`.
- A session that upgrades with STARTTLS is **accepted** and delivers.

**This is the re-evaluation property.** The upgrade creates a new session and
connect- and helo-stage policy runs again on it — otherwise the rule would refuse
the client before it had a chance to upgrade.

### 8. The connection is counted once

With the rule from step 7 removed, send one message over STARTTLS. Then check the
audit trail.

**Expected.** **One** Connection row for the whole TCP connection, not two —
despite the session being discarded and recreated by the upgrade.

Check `mailgw_messages_rejected_total` too, with the step-7 rule in place: a
refusal after an upgrade must count once, not twice.

### 9. A renewed certificate is picked up without a restart

Note the certificate's serial:

```bash
openssl x509 -in /tmp/tp-tls/server.crt -noout -serial -enddate
```

Generate a **new** pair to the same paths, then **without restarting**:

```bash
openssl s_client -connect localhost:4465 -quiet 2>&1 | openssl x509 -noout -serial
```

**Expected.** The new serial is served. The file's modification time is watched,
so certbot renewing in place works.

::: tip Changing the path does need a restart
What is watched is the file at the configured path. `restartRequired` compares
paths, never contents — so moving the certificate somewhere else is a restart.
:::

### 10. Managed mode generates a self-signed pair

*(Managed mode only.)* Deploy a `server` profile with `tls.cert` and `tls.key`
empty and `starttls: true`, to a node with no certificate.

**Expected.**
- The gateway generates a self-signed pair into its data directory.
- It **logs** that it is doing so, and says to replace the file in place for a
  real one.
- `STARTTLS` is advertised.

A file-mode gateway does **not** do this — verify that too, if you have both.

## Cleanup

Restore the original `server.yaml` and `ngmfilter.json`; restart.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 inert without keypair | | |
| 2 advertised with one | | |
| 3 upgrade delivers | | |
| 4 opt-out | | |
| 5 implicit TLS | | |
| 6 refusal readable inside TLS | | |
| 7 policy re-evaluated | | |
| 8 counted once | | |
| 9 renewal without restart | | |
| 10 self-signed fallback | | |
