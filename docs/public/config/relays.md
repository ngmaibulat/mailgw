# Relays

A **relay group** is a named list of destinations. Rules route a recipient to a
group; the delivery runner tries the group's members in priority order until one
accepts.

```json
{
    "Outbound": [
        { "name": "smtp-out-1", "exchange": "mx1.provider.net", "port": 587,
          "priority": 10, "auth_user": "user", "auth_pass_env": "RELAY_PASS",
          "tls": "required" },
        { "name": "smtp-out-2", "exchange": "mx2.provider.net", "port": 587,
          "priority": 20, "auth_user": "user", "auth_pass_env": "RELAY_PASS",
          "tls": "required" }
    ],
    "Exchange": [
        { "name": "exch-01", "exchange": "10.20.0.5", "port": 25, "priority": 10 }
    ]
}
```

## Fields

| Field | Default | Meaning |
|---|---|---|
| `name` | required | identifies this relay in logs and metrics |
| `exchange` | required | host to connect to, or a domain when `use_mx` is set |
| `port` | `25` | TCP port |
| `priority` | `0` | lower is tried first |
| `auth_user` | — | SMTP AUTH username; no AUTH is attempted without it |
| `auth_pass` | — | the password, literally |
| `auth_pass_env` | — | name of an environment variable holding it. **Wins over `auth_pass`** |
| `tls` | `opportunistic` | `none`, `opportunistic` or `required` |
| `allow_insecure_auth` | `false` | permit AUTH on an unencrypted connection |
| `use_mx` | `false` | resolve `exchange` as a domain via MX |

## Failover

Members are tried in `priority` order. A connection-level failure — dial, TLS,
AUTH or `MAIL FROM` — moves on to the next member. Once a relay has *accepted*
the transaction, its per-recipient answers are final for this attempt; the
gateway does not re-send accepted recipients to a second relay.

`outbound.per_group_connections` caps how many connections this gateway opens to
one group at a time.

## TLS policy {#tls}

| `tls` | Behaviour |
|---|---|
| `none` | never attempt STARTTLS |
| `opportunistic` | attempt STARTTLS, fall back to cleartext, **do not verify the certificate** |
| `required` | STARTTLS with full verification; refuse to deliver otherwise |

**`opportunistic` does not authenticate the peer, deliberately.** RFC 7435
opportunistic security is encryption without authentication: verifying under it
buys nothing, because the only thing on the other side of a verification failure
is a cleartext redial. If you need the guarantee, use `required` — which really
does verify, and really does refuse.

A downgrade — STARTTLS offered, attempted and failed, message sent in the clear —
is counted (`mailgw_delivery_tls_downgraded_total`) and logged. A relay that used
to encrypt and now does not shows up there and nowhere else.

TLS 1.2 is the floor in every mode. A `tls: required` relay still stuck on TLS
1.0 will fail.

## Credentials

Prefer `auth_pass_env`. It names a variable on the gateway's own host, so the
credential never enters a configuration bundle — and the console stores every
bundle version for ever.

::: warning A managed node has no environment
`auth_pass_env` is only usable in file mode, or on a node where you control the
container's environment. A centrally-managed node has no environment of its own,
which is exactly why the console decrypts stored passwords and puts the literal
in the bundle. That is a deliberate trade: the alternative — shipping ciphertext
and giving every gateway the key — would make one compromised edge node yield
every relay credential for the whole fleet.
:::

`check` warns about plaintext credentials so the choice is at least visible.

## use_mx {#use-mx}

With `use_mx`, `exchange` is treated as a **domain**: its MX records are looked
up at delivery time and expanded into one destination per exchanger, each
inheriting this relay's port, credentials and TLS policy. Equal-preference hosts
are shuffled per message, as RFC 5321 §5.1 asks.

Two things it is not:

- **It is not direct-to-MX.** The recipient's domain is never consulted. This is
  "smarthost named by domain" — what a relay with several MX hosts needs.
- **It is not free of trust implications.** With credentials configured, DNS now
  decides who receives them. `check` warns about that combination specifically.

A null MX record (RFC 7505) is honoured: that domain accepts no mail, and the
gateway does not try.

### What is cached, and for how long

Answers are held for `outbound.mx_cache_ttl` (5 minutes by default), because Go's
resolver does not expose record TTLs.

**Failures are held too, but only for 30 seconds**, and that number is fixed
rather than configurable. It exists for one case: `outbound.concurrency` workers
draining a queue against a domain whose DNS is unreachable would otherwise each
pay a fresh lookup and its full timeout, turning a slow resolver into an
unavailable one. Thirty seconds is below the shortest retry in the default
`outbound.backoff`, so a message retrying always re-resolves — this cache can
never be why a domain stays unreachable after its DNS is fixed. A SERVFAIL and a
timeout are treated the same.

A null MX is **not** in that 30-second cache. It is a permanent answer the domain
published deliberately, so it is held for the full `mx_cache_ttl` like any other.

None of this changes what you see when DNS fails: the gateway still logs
`cannot resolve mail exchangers` for every affected envelope, and the message
still defers and retries normally. Only the DNS traffic collapses.
