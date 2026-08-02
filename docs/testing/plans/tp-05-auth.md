# TP-05 · Inbound AUTH

**Purpose.** Verify that AUTH is offered only when it should be, that credentials
are checked correctly, that failures are permanent and countable, and that the
rule fields work.

**Duration.** ~30 minutes.

## Preconditions

- File mode, so you can edit `auth.json`.
- A TLS keypair available ([TP-04](/plans/tp-04-tls) passed).
- A bcrypt hash for a known password. Any bcrypt tool; from the console:

```bash
cd webui-fastify && node -e "import('bcryptjs').then(async b => \
  console.log(await b.default.hash('correct horse battery staple', 10)))"
```

## Steps

### 1. No credentials means no AUTH

Ensure `auth.json` does not exist. Restart.

```bash
swaks --server localhost:2525 --quit-after EHLO
```

**Expected.** No `AUTH` in the capability list. This is every deployment before
the feature existed.

### 2. Credentials without TLS still means no AUTH

`auth.json`:

```json
{ "users": [{ "user": "app@example.com", "hash": "$2b$10$…" }] }
```

with `tls.cert`/`tls.key` empty and `allow_insecure_auth` unset. Restart.

**Expected.**
- No `AUTH` advertised.
- **A warning at startup** saying credentials are configured but AUTH can never
  be advertised.

**Record the warning text.** This configuration looks entirely correct and cannot
work; the warning is the only thing that says so.

### 3. AUTH is refused on the wire too, not merely unadvertised

```bash
printf 'EHLO probe\r\nAUTH PLAIN AGFwcEBleGFtcGxlLmNvbQBwYXNz\r\nQUIT\r\n' \
    | nc localhost 2525
```

**Expected.** `523 5.7.10` — TLS is required.

A client that tries anyway must not be able to put a password on a cleartext
socket.

### 4. allow_insecure_auth enables it deliberately

```yaml
tls:
    allow_insecure_auth: true
```

Restart.

**Expected.**
- `250-AUTH PLAIN LOGIN` in the capability list.
- A **warning** that submission passwords may cross the network in the clear.

### 5. A correct credential succeeds

```bash
swaks --server localhost:2525 \
      --auth PLAIN --auth-user app@example.com \
      --auth-password 'correct horse battery staple' \
      --from app@example.com --to b@ngm.dev
```

**Expected.** `235 2.0.0` after AUTH, then the message delivers.

### 6. A wrong password is refused permanently

```bash
swaks --server localhost:2525 --auth PLAIN --auth-user app@example.com \
      --auth-password 'wrong' --quit-after AUTH
```

**Expected.** **`535 5.7.8`** — not `454`.

**This is the property being tested.** A `4xx` would tell the client the failure
is temporary and invite it to retry the same wrong password for ever.

### 7. An unknown username behaves identically

```bash
swaks --server localhost:2525 --auth PLAIN --auth-user nobody@example.com \
      --auth-password 'anything' --quit-after AUTH
```

**Expected.** The same `535 5.7.8`, and — as far as you can tell — the same
response time as step 6.

**Time both** if you can. An unknown username that returns noticeably faster turns
AUTH into a username oracle.

### 8. LOGIN works as well as PLAIN

```bash
swaks --server localhost:2525 --auth LOGIN --auth-user app@example.com \
      --auth-password 'correct horse battery staple' --quit-after AUTH
```

**Expected.** `334` challenges for username and password, then `235`.

### 9. An unsupported mechanism is refused

```bash
printf 'EHLO probe\r\nAUTH CRAM-MD5\r\nQUIT\r\n' | nc localhost 2525
```

**Expected.** A refusal, not a challenge. CRAM-MD5 would require the server to
hold a recoverable password, which storing hashes rules out.

### 10. Counters move

```bash
curl -s http://localhost:8080/metrics | grep mailgw_auth
```

**Expected.** `mailgw_auth_succeeded_total` and `mailgw_auth_failed_total` match
what you did. They count per **AUTH command**, so step 5's session — one AUTH,
one message — adds exactly one success.

### 11. Over TLS, without allow_insecure_auth

Set `allow_insecure_auth: false`, configure a keypair, restart.

**Expected.**
- No `AUTH` before `STARTTLS`.
- `AUTH` **is** advertised after the upgrade.
- Authenticating on the encrypted session works.

### 12. Authentication does not survive STARTTLS

With `allow_insecure_auth: true` **and** a keypair, so AUTH is available both
before and after: authenticate on the plain session, then `STARTTLS`, then send a
message.

**Expected.** The upgraded session is **anonymous** — a rule requiring
authentication (step 14) refuses it until you authenticate again.

**This is correct, not a bug.** A credential presented in the clear must not
carry into the encrypted session.

### 13. It survives RSET

Authenticate, then send two messages with an `RSET` between them.

**Expected.** Both accepted. The identity is connection-scoped.

### 14. Rules see the authenticated user

```yaml
policy:
    - name: require-auth
      priority: 100
      match: {field: auth.authenticated, op: eq, value: false}
      then:
          - {action: reject, code: 530, message: "5.7.0 authentication required"}
```

Restart.

**Expected.**
- Unauthenticated: `530` at `MAIL FROM` — **not** at `EHLO`.
- Authenticated: `250` and delivery.

**The stage matters.** `auth.*` is a mail-stage field because AUTH happens after
`EHLO` is answered; a helo-stage rule could never see it.

Confirm with `check`: the rule's inferred stage is **mail**.

### 15. explain can model it

```bash
go run ./cmd/mailgw-go explain -config /tmp/tp-config \
    --rcpt b@ngm.dev --from a@x.com
go run ./cmd/mailgw-go explain -config /tmp/tp-config \
    --rcpt b@ngm.dev --from a@x.com -auth-user app@example.com
```

**Expected.** The first shows `require-auth` **matched** and the recipient
refused; the second shows it **not matched** and the route resolved.

### 16. A password where a hash belongs fails the load

```json
{ "users": [{ "user": "app@example.com", "hash": "correct horse battery staple" }] }
```

```bash
go run ./cmd/mailgw-go check -config /tmp/tp-config
```

**Expected.** Non-zero exit, saying the value is not a bcrypt hash.

**This is the mistake worth catching.** A plaintext password there would look
like a working configuration that simply never lets anyone in.

### 17. Revocation takes effect immediately

*(Managed mode.)* With a working credential, remove the user from the credential
set in the console and deploy.

**Expected.** The next AUTH attempt with that credential is refused, **without a
gateway restart**. Credentials are re-read per command.

## Cleanup

Remove `auth.json`, restore `server.yaml` and `routing.yaml`, restart.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 no creds, no AUTH | | |
| 2 no TLS, warns | | |
| 3 523 on the wire | | |
| 4 allow_insecure_auth | | |
| 5 success | | |
| 6 wrong password 535 | | |
| 7 unknown user identical | | |
| 8 LOGIN | | |
| 9 unsupported mechanism | | |
| 10 counters | | |
| 11 over TLS | | |
| 12 discarded by STARTTLS | | |
| 13 survives RSET | | |
| 14 rule fields | | |
| 15 explain | | |
| 16 password as hash refused | | |
| 17 revocation | | |
