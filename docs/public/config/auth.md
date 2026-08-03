# Inbound SMTP AUTH

Without credentials, the [IP allowlist](/config/allowlist) is the only thing
deciding who may send through the gateway. AUTH adds a second answer: *this
client proved who it is*, whatever address it came from.

## Configuring credentials

In file mode, `auth.json`:

```json
{
    "users": [
        { "user": "app@example.com", "hash": "$2b$10$Xk9…" },
        { "user": "billing-service", "hash": "$2b$10$Qz2…" }
    ]
}
```

In managed mode, a **credential set** in the console. Create the set, add users
with a password, assign the set to a gateway, deploy.

**The `hash` is a bcrypt hash, never a password.** The console hashes on save; in
file mode you produce it yourself. A password pasted where a hash belongs fails
the configuration load rather than becoming a credential that silently never
works.

Usernames are compared **case-sensitively**. A submission credential is an opaque
string you issued, not a mailbox.

## When AUTH is offered

Three independent conditions, and all three are off in a configuration that says
nothing:

1. at least one credential is configured;
2. the session is encrypted, **or** [`tls.allow_insecure_auth`](/config/tls#allow_insecure_auth) is set;
3. — there is no third switch. Those two are it.

If credentials exist but neither TLS nor `allow_insecure_auth` does, `AUTH` is
never advertised and the gateway **warns at startup**, because that configuration
looks entirely correct and cannot work.

## Mechanisms

`PLAIN` and `LOGIN`. Between them they cover every submission client that will
meet this gateway.

`CRAM-MD5` and the other challenge-response mechanisms are **not** implemented
and will not be: they require the server to hold a recoverable password, which
is exactly what storing hashes rules out.

## Replies

| Situation | Reply |
|---|---|
| success | `235 2.0.0` |
| wrong username or password | `535 5.7.8` |
| AUTH attempted on a cleartext session without `allow_insecure_auth` | `523 5.7.10` |
| unknown mechanism | `454 4.7.0` |

A bad credential is **permanent** (`535`), not temporary. Answering `4xx` would
tell a client to come back and try the same wrong password again.

An unknown username costs exactly as much time as a known one with a wrong
password, so AUTH cannot be used to enumerate usernames.

## Using it in rules

This is the point of the feature. Authentication does not *by itself* grant
anything — it sets facts your rules read:

| Field | Type | Meaning |
|---|---|---|
| `auth.authenticated` | bool | the client completed AUTH |
| `auth.user` | string | the username, empty when unauthenticated |
| `auth.mechanism` | string | `PLAIN` or `LOGIN` |

So "authenticated senders may relay anywhere, everyone else is allowlist-only"
is a rule you write:

```yaml
policy:
    - name: submission-requires-auth
      priority: 10
      match:
          all:
              - {field: conn.remote_port, op: eq, value: 587}
              - {field: auth.authenticated, op: eq, value: false}
      then:
          - {action: reject, code: 530, message: "5.7.0 authentication required"}

routes:
    - name: authenticated-senders-relay-out
      priority: 20
      match: {field: auth.authenticated, op: eq, value: true}
      then:
          - {action: relay, relay: Outbound}
```

::: tip These are mail-stage fields
AUTH happens after `EHLO` is answered, and connect- and helo-stage policy has
already run by then. So a rule reading `auth.*` is evaluated at `MAIL FROM` — the
earliest point at which the answer is reliable. A rule that combined `auth.user`
with a connect-stage field would still fire at `mail`.
:::

Test a rule without sending anything:

```bash
mailgw-go explain --rcpt b@partner.com --from a@x.com \
    -auth-user app@example.com
```

## Behaviour worth knowing

**Authentication does not survive STARTTLS.** A client that authenticates on the
plain session and *then* upgrades starts the encrypted session anonymous, because
the upgrade discards the session entirely. This is correct — a credential
presented in the clear must not carry into the encrypted session — and correct
clients authenticate after the upgrade anyway.

**It survives `RSET`.** The identity is connection-scoped; a client may send
several messages on one authenticated connection.

**Revocation takes effect on the next AUTH command.** Credentials are re-read
per command rather than snapshotted per session, so removing one and deploying
stops it working immediately rather than at the next restart.

## Monitoring

| Metric | Counts |
|---|---|
| `mailgw_auth_succeeded_total` | successful AUTH commands |
| `mailgw_auth_failed_total` | AUTH commands refused `535` |

Per **command**, not per connection or message: one authenticated session sending
fifty messages counts once.

Alert on the *rate* of `mailgw_auth_failed_total`, not the total. It is where a
credential-stuffing run against the submission port shows up.

::: warning There is no rate limiter yet
Nothing throttles repeated failed AUTH attempts. Concurrent password checks are
bounded so a guessing run cannot starve the delivery path of CPU, but that is a
floor under the damage rather than a limiter. Keep the submission port
firewalled to the senders that need it.
:::
