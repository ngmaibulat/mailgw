# Architecture overview

Four services and a frozen predecessor.

```
        SMTP :25/:465                 HTTPS
   senders ──────────► mailgw-go ────────────► webui-fastify
                          │  ▲                  (console)
                    audit │  │ config bundles       │
                          ▼  │                      │ Drizzle
                      logservice ◄──────────────────┤
                          │      reads proxied      │
                          ▼                         ▼
                       MariaDB ◄────────────────────┘
```

## The pieces

**`mailgw-go`** — the gateway. A single Go binary: SMTP server, rule engine,
outbound spool, delivery runner, audit event pipeline, and a small local admin
UI. It owns port 25. Everything that touches live mail is here.

**`logservice`** — a Bun HTTP service over MariaDB. Receives audit events and
serves searches. It is the only thing that owns the log schema and the only thing
that migrates it.

**`webui-fastify`** — the admin console. Two jobs that share a process: log
viewers for operators, and **Central Management** for the gateway fleet
(inventory, approval, configuration profiles, versioned deploy and rollback).

**`legacy/`** — the Haraka implementation this replaced, plus its Express console.
Frozen. See [legacy/](/packages/legacy).

## Why the gateway is Go and the rest is not

The gateway is the only component with hard latency and durability requirements —
it sits in the SMTP path, it owns the spool, and its failure modes lose mail. A
single static binary with no runtime, no package manager and no dependency
surface is worth a lot there.

The console and log service are I/O-bound web services where developer speed
matters more than milliseconds. They stayed on the runtimes they were written in.

## Who owns what data

This is the single most useful thing to know before changing anything:

| Schema | Owned by | Migrated by |
|---|---|---|
| Log tables, config tables | **logservice** | `logservice/migrations/*.sql` |
| Gateway identity, config cache | **mailgw-go** | `internal/store` migrations |

`webui-fastify` **describes** the tables it queries in `db/schema.ts` but does not
own or migrate them. There is no `drizzle-kit` in the console for exactly this
reason. Adding a column means: write the SQL migration in `logservice`, then
describe it in the console's schema file.

## How configuration reaches a gateway

Two modes, one set of validators.

**File mode** (`-config <dir>`) reads a directory. It is what `check`, `explain`,
the contract test suite and the Bun end-to-end suite all run on, and the
development compose file pins it for that reason.

**Managed mode** (no arguments) pulls a JSON bundle from the console into a local
SQLite cache. The bundle's keys mirror the configuration directory's filenames
one for one — that is the whole design, and it is what lets `check` and `explain`
work identically in both modes.

Both paths converge on one `*config.Config` through the same validators, so a
configuration that passes in one behaves the same in the other. See
[Central Management](/architecture/central-management).

## The through-lines

**Fail closed on the inbound gate.** A missing or unparseable allowlist denies
everyone and refuses to start. The zero value denies, so even a code path that
ignored a load error could not open the relay.

**Never lose mail; duplicating is acceptable.** Everything in the spool is
committed with `rename(2)`. Where a choice exists between losing a message and
sending it twice, it sends it twice — and where a bookkeeping shortcut could
delete a live body, the shortcut is not taken.

**The mail path never waits on management.** Registration, config polling and
heartbeats are as failure-tolerant as the audit pipeline. If the console is down,
mail keeps flowing on the last good configuration, and `/readyz` deliberately
does not consult it.

**Say what is wrong, by name.** A configuration that loads but cannot work
produces a specific warning. "Restart required" is reported as a list of what
changed, not a bare boolean, because an alarm nobody can act on is not an alarm.

**Validation belongs to the thing that will run it.** The console does shape
checks; the gateway compiles the rule language, keeps its last good configuration
on failure, and reports the compiler's own message back.

See [Standing decisions](/architecture/decisions).
