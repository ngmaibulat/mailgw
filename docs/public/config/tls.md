# TLS

Inbound TLS covers two separate things: **STARTTLS** on a plain port, and
**implicit TLS** on a dedicated one.

```yaml
tls:
    cert: /var/lib/mailgw-go/tls/server.crt
    key: /var/lib/mailgw-go/tls/server.key
    starttls: true
    allow_insecure_auth: false
```

## starttls

**Defaults to `true`, so it is an opt-out.** But it is inert without a keypair,
so a configuration that never mentions TLS behaves exactly as it always did. A
gateway that has a certificate should offer encryption, and a sender that cannot
use it simply will not.

Set it `false` for a submissions-only node whose plain port must stay plain.

## implicit_tls

Set per listener, not here:

```yaml
listen:
    - addr: "0.0.0.0:465"
      implicit_tls: true
```

The socket is wrapped in TLS from the first byte. Needs `cert` and `key`.

## Certificates

`cert` and `key` are **paths on the gateway's own filesystem**. They are never
material carried in a configuration bundle: the console keeps every version for
ever and serves it to every gateway assigned that profile, so a private key
placed there would be permanently retained and fleet-wide.

A certificate **renewed in place is picked up without a restart** — the file's
modification time is watched. An external certbot on the host therefore works
today. What is watched is the path, so changing the *path* does need a restart.

### Self-signed fallback

A **centrally-managed** node with no keypair configured generates a self-signed
pair into its data directory. That is a substitute for cleartext, not for a real
certificate: it makes the session encrypted, which is the point, but any peer
that verifies names will refuse it.

A file-mode gateway does not do this. It has an operator who can put a
certificate beside its configuration, and inventing files next to somebody's
configuration directory is not the program's business.

## allow_insecure_auth

Whether `AUTH` may be offered on an **unencrypted** session. Defaults to `false`,
and without it a client that tries anyway is answered `523 5.7.10`.

It lives here rather than with the credentials because it is a property of the
listener, not of any user — and because it is read once when the server is built,
so changing it needs a restart while credentials themselves hot-swap.

The legitimate case is TLS terminated in front of the gateway. If that is you,
set [PROXY protocol](/config/server#listeners) on the listener as well, or the
allowlist and the `conn.*` rules see the terminator instead of the client.

See [Inbound AUTH](/config/auth).

## Reading it in rules

```yaml
- name: require-tls-from-partners
  match:
      all:
          - {field: conn.remote_ip, op: in_cidr, value: "203.0.113.0/24"}
          - {field: conn.tls, op: eq, value: false}
  then:
      - {action: reject, code: 530, message: "5.7.0 encryption required"}
```

`conn.tls`, `conn.tls_version` and `conn.tls_cipher` are helo-stage fields.

::: tip Policy is re-evaluated after a STARTTLS upgrade
go-smtp discards the session and creates a new one when a client upgrades, so
connect- and helo-stage rules run again on the encrypted session — which is what
makes the rule above work. The gateway makes sure the connection is still counted
and reported only once.
:::

## Outbound TLS

Per relay, not here. See [Relays](/config/relays#tls).
