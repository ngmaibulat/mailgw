# Getting started

## Prerequisites

| Tool | Used by |
|---|---|
| Go 1.26+ | `mailgw-go` |
| Node 26+ | `webui-fastify`, these docs sites |
| pnpm 11 | the workspace (pinned in `packageManager`) |
| Bun | `logservice`, `tests`, `certs` |
| Docker | the full stack |

## First run

```bash
pnpm install                 # webui-fastify + docs/*
cd logservice && bun install && cd ..
cd certs && bun install && cd ..
pnpm certs                   # the console will not boot without these
```

Then the gateway on its own, which needs nothing else running:

```bash
cd mailgw-go
go run ./cmd/mailgw-go check -config ./testdata/config
go run ./cmd/mailgw-go serve -config ./testdata/config
```

::: warning Point the spool somewhere writable
`testdata/config/server.yaml` has `outbound.spool_dir: /opt/mailgw-go/queue`,
which an unprivileged user cannot create. Change it, or the gateway exits with
`cannot open spool`.
:::

Or the whole stack:

```bash
docker compose up            # mariadb, logservice, mailgw-go, webui, mailhog
```

## The loop

```bash
cd mailgw-go
gofmt -l .                   # must print nothing
go vet ./...
go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config
```

```bash
cd webui-fastify
pnpm typecheck
pnpm check                   # biome lint + format
pnpm test
```

All of it, from the root:

```bash
pnpm test                    # go test ./... + the logservice suite
```

## Things that will confuse you once

**`pnpm --filter @aibulat/mailgw` does not resolve.** The Haraka package is
outside the workspace.

**Do not run pnpm scripts against the Bun projects.** `logservice`, `tests` and
`certs` have their own `bun.lock` and are excluded on purpose.

**The console has no build step**, so imports carry `.ts` extensions and TS
features that need emit are unavailable.

**`mailgw-go/container-build.sh` does not bump the version**, unlike every Node
one. Use `./bump.sh`.

**End-to-end tests run from the repository root** so Bun picks up the root
`.env`.

## Making a change to the mail path

1. Read the relevant `plans/M<n>-*.md` — the reasoning is usually there already.
2. Write the test first, and **check it fails without the fix**. A green test
   that would also be green without your change proves nothing.
3. `go test -race ./...`, then `check`, then the Bun SMTP suite.
4. If it changes behaviour on the wire, say so in `CLAUDE.md`.

## Making a change to configuration shape

Anything the gateway reads has **two** sources — a file and a bundle key — and
both must move together:

1. `internal/config`: the struct, the file load, validation.
2. `internal/config/bundle.go`: the bundle field, and `RedactBundle` if it is a
   secret.
3. `cmd/mailgw-go/configcmd.go`: so `config show` covers it in file mode.
4. `cmd/mailgw-go/gateway.go`: `restartRequired`, unless it hot-swaps.
5. The console: `bundle.ts`, and a migration if it is stored.
6. Both sample `server.yaml` files.

Miss step 4 and a deployed change reports "applied" while the process keeps
running the old value.

## Adding a rule field

1. `internal/ruleset/schema.go` — name, stage, type, description.
2. `internal/ruleset/env.go` — a `Lookup` arm.
3. Populate it, wherever the fact becomes known.
4. `cmd/mailgw-go/env.go` if `explain` should be able to model it.

**Choose the stage carefully.** It is the earliest stage at which the answer is
*reliable*, not the earliest at which the field is conceptually meaningful — a
field declared too early is a rule that can never match.
