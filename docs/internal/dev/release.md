# Release

## Versions

Each package versions independently, in its own `package.json` or `bump.sh`.
There is no repository-wide version.

## The modern stack

```bash
pnpm run docker:push
```

That is the release command: `mailgw-go` + `logservice` + `webui-fastify`, the
three images a modern deployment actually runs. It bumps all three versions
first — the two Node packages bump inside their own `container-push.sh`, and it
calls `mailgw-go/bump.sh` explicitly because that script's push deliberately
never mutates the tree.

The steps are `&&`-chained, so a failed build aborts the rest rather than leaving
a half-pushed `:latest`.

Legacy Haraka and the Express console are **not** included:

```bash
pnpm build:containers              # the above, plus the legacy Haraka image
./legacy/webui-express/container-build.sh
```

## Before pushing

```bash
cd mailgw-go && gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config
cd ../webui-fastify && pnpm typecheck && pnpm check && pnpm test
cd ../logservice && bun test tests/
```

Then the end-to-end suites against a running stack, and the
manual test plans (`docs/testing`) — at minimum TP-01, and whichever plans cover
what changed.

## Deploying

**Core** (one per installation):

```bash
cd deploy/core && bash upgrade.sh     # pull images, re-run migrations
```

**Edge gateways**: pull and restart. Configuration comes from the console, so
there is nothing to copy.

## Ordering, and the trap

Run **migrations before** the services that need them. `upgrade.sh` does this.

::: danger An image rollback after a store schema bump bricks a gateway
`internal/store` migrations are forward-only and `store.Open` refuses a database
newer than the binary. Rolling a gateway image back past a store migration means
the node will not start, and recovery is replacing its data volume — which
destroys its identity and forces re-registration and re-approval.

Both compose files pin `:latest`, so `docker compose pull` is unversioned and an
upgrade is not repeatable. This is a known gap; pin a tag on anything you care
about being able to roll back.
:::

The same hazard does not apply to the log database: those migrations are additive
and older services tolerate columns they do not read.

## Configuration compatibility

A bundle carries `format: 1`. A gateway that does not recognise the format
**refuses the bundle and keeps its last good configuration**, which is visible in
the console as an apply error — deliberately better than guessing.

Bump the format only when the document's shape changes, and never for a new
optional key.

Spool envelopes carry their own version, and adding a field there is
`omitempty`-only at version 1. See
[the mail path](/architecture/mail-path#spool).

## Known gaps in the release process

Recorded so they are not rediscovered:

- **CI covers the Go module only**, and the image is pushed by a human running a
  script.
- **No log rotation anywhere.** Neither compose file sets a logging driver with
  `max-size`, and Docker's default `json-file` is unbounded — on a busy relay the
  container log grows until the disk fills.
- **No healthchecks in compose**, despite `/healthz` existing. Not free to add:
  the runtime image is `distroless/static` with no shell and no `curl`, and the
  binary has no `healthcheck` subcommand. Adding one is the cheap fix.
- **No resource limits** in any compose file.
- **No backup tooling.** `deploy/README.md` says to back up
  `/opt/mailgw-go/data` and nothing does it; a naive `cp` of the SQLite file is
  unsafe in WAL mode. The **spool** is not mentioned as backup-worthy at all,
  though it holds undelivered, quarantined and dead mail.
