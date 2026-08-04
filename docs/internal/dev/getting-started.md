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
pnpm certs                   # optional: the console self-signs a pair if you skip it
```

Then the gateway on its own:

```bash
cd mailgw-go
go run ./cmd/mailgw-go serve -data /tmp/mailgw-dev
```

::: warning It will not relay until it is provisioned
There is no way to hand a gateway a configuration from this host — Central
Management is the only source. Run with no bundle and it boots empty, generates
an identity and waits at `http://localhost:8080` with a claim code in its log.

To exercise the mail path, bring up the whole stack and provision it (below)
rather than running the binary alone. To exercise the *rule engine* without a
console, `go test ./internal/ruleset/` is the faster loop.
:::

Or the whole stack:

```bash
docker compose up -d         # mariadb, logservice, mailgw-go, webui, mailhog
pnpm provision               # drive the console: profiles, approve, deploy
```

`pnpm provision` is what replaces the config directory the dev stack used to
mount. It is idempotent. See `tests/README.md`.

## The loop

```bash
cd mailgw-go
gofmt -l .                   # must print nothing
go vet ./...
go test -race ./...
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
3. `go test -race ./...`, then the Bun SMTP suite against a provisioned stack.
4. If it changes behaviour on the wire, say so in `CLAUDE.md`.

## Making a change to configuration shape

Anything the gateway reads comes from the bundle, and nowhere else:

1. `internal/config`: the struct and its validation.
2. `internal/config/bundle.go`: the bundle field, and `RedactBundle` if it is a
   secret.
3. `cmd/mailgw-go/gateway.go`: `restartRequired`, unless it hot-swaps.
4. The console: `bundle.ts`, and a migration if it is stored.
5. `docs/public/config/` — the reference is where an operator finds the key now
   that there is no sample file to read.

Miss step 3 and a deployed change reports "applied" while the process keeps
running the old value.

**Never add a key that names an environment variable.** The gateway reads none,
so it can only ever resolve to the empty string. `auth_pass_env` did exactly
that and authenticated against relays with an empty password; it is now refused
at load time.

## Adding a rule field

1. `internal/ruleset/schema.go` — name, stage, type, description.
2. `internal/ruleset/env.go` — a `Lookup` arm.
3. Populate it, wherever the fact becomes known.
4. `cmd/mailgw-go/env.go` if `explain` should be able to model it.

**Choose the stage carefully.** It is the earliest stage at which the answer is
*reliable*, not the earliest at which the field is conceptually meaningful — a
field declared too early is a rule that can never match.
