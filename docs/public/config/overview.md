# Configuration overview

A gateway's configuration is a directory of files. In file mode you edit them; in
managed mode the console composes exactly the same set and delivers it as a
bundle. The names and the contents are identical either way, so anything you
learn here applies to both.

| File | Required | What it holds |
|---|---|---|
| [`server.yaml`](/config/server) | no | listeners, limits, timeouts, outbound tuning, TLS paths |
| [`relays.json`](/config/relays) | **yes** | relay groups and their members |
| [`ngmfilter.json`](/config/allowlist) | **yes** | the IP allowlist |
| `routing.yaml` | no | your policy and routing rules |
| `logging.json` | no | where audit events are posted |
| `auth.json` | no | [inbound SMTP AUTH](/config/auth) credentials |
| `admin.json` | no | the bearer token for `/metrics` and `/readyz` |

Only two are required. A gateway with no `relays.json` cannot deliver anything,
and one with no allowlist would have no inbound gate — so both are hard errors
rather than empty defaults.

`routing.json` is also accepted, in the older four-field Haraka format, and is
transpiled into the same compiled ruleset. `routing.yaml` wins if both exist.

## Validate before you serve

```bash
mailgw-go check -config ./config
```

It exits non-zero on error and prints what it understood: the hostname, the
listeners, the allowlist, the relay groups, every rule with its inferred stage,
and the default action. Warnings cover the configurations that load but will not
do what you meant:

- an allowlist with `allow_all` set
- relay passwords in plaintext
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

## Precedence

For the settings that can come from more than one place:

- **Relay password**: `auth_pass_env` (a variable name to look up) beats
  `auth_pass` (a literal), because the literal is the one that ends up in a
  bundle.
- **Log service API key**: `logging.api_key` beats `events.api_key_env`. An
  explicit value beats a name to look up.
- **Spool directory**: an explicit `outbound.spool_dir` always wins. Only when it
  is left at the compiled-in default does a managed node substitute its own data
  directory.
