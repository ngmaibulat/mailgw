# Command reference

```
mailgw-go <command> [flags]
```

The gateway is **centrally managed** and has no other configuration source: it
stores its identity and configuration cache under `-data`, is provisioned
through the admin UI, and is told everything else by the console. `check`,
`explain`, `mailq`, `events` and `config show` all read that same cache, so they
answer questions about what this gateway is actually running.

## serve

```bash
mailgw-go serve                        # or just: mailgw-go
```

The default command — `mailgw-go` with no subcommand serves.

`SIGHUP` reloads the allowlist and the rules, all or nothing.
`SIGTERM`/`SIGINT` begins an ordered shutdown bounded by
`server.shutdown_timeout`: stop listening, drain sessions, finish the delivery in
flight, flush audit events.

## check

```bash
mailgw-go check
mailgw-go check -data /var/lib/mailgw-go     # what a managed node is running
```

Validates and prints what it understood. **Non-zero exit on error.** Run it in CI
and before every restart; it is the cheapest test in the system.

## explain

```bash
mailgw-go explain --rcpt bob@partner.com --from alice@example.com
```

Answers "why would this message go there?" without sending anything.

| Flag | Default | Meaning |
|---|---|---|
| `--rcpt` | required | recipient to evaluate |
| `--from` | `""` | envelope sender |
| `--ip` | `127.0.0.1` | client address |
| `--helo` | `client.invalid` | `EHLO` name |
| `--stage` | `data` | evaluate at `connect`\|`helo`\|`mail`\|`rcpt`\|`data` |
| `--eml` | — | a message file, to populate data-stage fields |
| `--tls` | off | treat the session as encrypted |
| `--auth-user` | — | treat the session as authenticated as this user |
| `--auth-mech` | `PLAIN` | mechanism to report, with `--auth-user` |
| `--spf` | — | SPF result to assume: `pass`\|`fail`\|`softfail`\|`neutral`\|`none`\|`temperror`\|`permerror` |
| `--dkim` | — | DKIM result to assume |
| `--dmarc` | — | DMARC result to assume |
| `--dmarc-policy` | — | policy the `From` domain publishes, with `--dmarc` |

It prints every rule, whether it matched, at what stage, and the outcome.

The message-authentication flags **fake** the results rather than resolving
them, on the same footing as `--tls` and `--auth-user`: the useful question is
"what would my rules do if DMARC failed", and `explain` performs no network I/O
at all — so it runs on a laptop against a bundle for a gateway on another
continent. Leaving one unset means that check did not run, which is what the
fields themselves mean.

```bash
mailgw-go explain --rcpt you@ngm.dev --dmarc fail --dmarc-policy reject
```

## fields

```bash
mailgw-go fields
```

Every field rules can match on, with its stage, type and description. Fields this
build never populates are flagged.

## mailq

```bash
mailgw-go mailq [-json]              # list everything
mailgw-go mailq flush [uuid...]      # make due now
mailgw-go mailq rm <uuid>...         # delete, collecting bodies
mailgw-go mailq release <uuid>...    # quarantine -> queue
mailgw-go mailq hold <uuid>...       # queue -> quarantine
```

An envelope currently being delivered cannot be flushed, removed or held. See
[The queue](/operations/queue).

## events

```bash
mailgw-go events [-json] [-all]      # what is parked
mailgw-go events replay              # resend, oldest first
mailgw-go events rm <file>...        # delete without sending
```

`-all` includes `rejected/` — events a replay gave up on permanently.

## config show

```bash
mailgw-go config show -data /var/lib/mailgw-go
```

Prints the cached configuration bundle, with relay passwords, the log service API
key, the metrics token and inbound credential hashes **redacted**. The
managed-mode equivalent of `cat routing.yaml`.

It works in file mode too, composing the same document from the files on disk, so
the command answers the same question in both modes.

## claim

```bash
mailgw-go claim status -data /var/lib/mailgw-go   # show the code
mailgw-go claim reset  -data /var/lib/mailgw-go   # rotate it, sign everybody out
```

`status` does **not** rotate — it is the answer to "I lost the code", without
signing out every other operator. `reset` mints a new one and revokes every
session in a single transaction.

## convert-routing

```bash
mailgw-go convert-routing config/routing.json > config/routing.yaml
```

Transpiles the older Haraka four-field table into the rule DSL. The output is
equivalent to the table it came from; edit it freely afterwards.

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `-data <dir>` | `/var/lib/mailgw-go` | identity and configuration cache |
| `-admin <addr>` | `0.0.0.0:8080` | admin UI bind address |
| `-version` | — | print the version and exit |

These are the whole command line, and both defaults are what a shipped node
runs on — the container image has no `CMD` at all. Neither is *configuration*:
`-data` is where a configuration is cached and `-admin` is how a node that has
no configuration yet gets one, which is why neither can arrive in a bundle.
