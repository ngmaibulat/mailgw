# Configuration overview

A gateway's configuration is composed by the console from **config profiles**
you author there, and delivered as one signed bundle. Nothing is read from the
gateway's own filesystem.

The sections below are named after files because the bundle's keys still are —
that mapping is the whole design, and it is what makes a profile's contents
diffable against what a gateway is running. Author them at
`/config/profiles/create`; the *kind* you pick decides which key it becomes.

| Key | Profile kind | Required | What it holds |
|---|---|---|---|
| [`server.yaml`](/config/server) | `server` | no | listeners, limits, timeouts, outbound tuning, TLS paths |
| [`relays.json`](/config/relays) | *relay groups* | **yes** | relay groups and their members |
| [`ngmfilter.json`](/config/allowlist) | `allowlist` | **yes** | the IP allowlist |
| `routing.yaml` | `ruleset` | no | your policy and routing rules |
| `logging.json` | — | no | where audit events are posted |
| `auth.json` | *credential set* | no | [inbound SMTP AUTH](/config/auth) credentials |
| `admin.json` | — | no | the bearer token for `/metrics` and `/readyz` |

Only two are required. A gateway with no relays cannot deliver anything, and one
with no allowlist would have no inbound gate — so both are hard errors rather
than empty defaults.

Three of these are not profiles you write. Relay groups and credential sets are
database rows with their own screens, and `logging.json` and `admin.json` are
composed from the console's own settings — a gateway has no environment to read
them from.

The older four-field Haraka `routing.json` is still accepted by
`mailgw-go convert-routing`, which transpiles it to `routing.yaml` for pasting
into a `ruleset` profile.

## Check what a gateway is running

```bash
mailgw-go check          # on the gateway itself
```

It reads the bundle this gateway has cached — not a file you are about to deploy
— so it answers "what is actually in force here?". It exits non-zero on error
and prints what it understood: the hostname, the
listeners, the allowlist, the relay groups, every rule with its inferred stage,
and the default action. Warnings cover the configurations that load but will not
do what you meant:

- an allowlist with `allow_all` set
- relays carrying a password
- credentials sent to MX-resolved hosts, where DNS decides who receives them
- `dsn.enabled` with no `dsn.relay_group`, so a bounce that no rule claims cannot be sent
- rules matching on a field this build never populates
- inbound credentials configured where `AUTH` can never be advertised
- no route rules at all

## Reloading

`SIGHUP` reloads the **allowlist and the rules** — the two things that can change
under a running process — and it is all-or-nothing: if the new configuration
fails validation, the running one stays in force and the failure is logged.

```bash
kill -HUP $(pidof mailgw-go)
```

Everything else needs a restart. That is not an oversight: listeners are bound,
the TLS keypair is loaded, the relay table is held by the delivery runner, and
the spool directory is open. A change to any of those is reported by name so you
know what a restart would pick up.

## Reloading, and `SIGHUP`

`SIGHUP` re-applies the cached bundle, which is the same thing a deploy does.

## There is no precedence

Every setting has exactly one source: the deployed bundle. This section used to
list which of two sources won for a relay password, the log service API key and
the spool directory — a sign that the second source was a liability rather than a
convenience, and all three are gone.

Two consequences worth stating:

- **`auth_pass_env` is refused**, not ignored. A gateway reads no environment, so
  it could only ever resolve to an empty password — which a relay reports as a
  *wrong* credential, sending you to check an account that was never used. Set
  the password on the relay in the console; it is encrypted at rest and
  decrypted only when a bundle is composed.
- **The spool directory** is `outbound.spool_dir` when the server profile names
  one, and otherwise a directory under `-data` that the gateway is guaranteed to
  be able to write. See [the queue](/operations/queue#where-spool-dir-actually-is).
