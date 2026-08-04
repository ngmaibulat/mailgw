# logservice (Bun) — FROZEN

**This package is frozen. The live service is [`logservice-go/`](../../logservice-go/).**

Both compose files now run `ngmaibulat/logservice-go:latest`. This one is kept
buildable for one release so that a rollback is an image-tag edit —

```yaml
logservice:
    image: ngmaibulat/logservice:latest
db-migrator:
    image: ngmaibulat/logservice:latest
    command: ["bun", "src/dbmigrate.ts"]
```

— rather than a revert. It now sits under `legacy/` for that reason — the same precedent as
`legacy/mailgw` (Haraka) and `legacy/webui-express`.

## What replaced it, and what did not change

`logservice-go/` is wire-compatible: the same routes, the same request and
response bodies, the same `q` search semantics including the behaviours that look
like bugs (an unknown field is skipped silently; a malformed `q` yields the
defaults; a falsy value is dropped except `0`). `tests/api/logservice.e2e.test.ts`
runs **unmodified** against it.

Two things did change, both deliberate and both documented in
[docs/internal/packages/logservice.md](../../docs/internal/packages/logservice.md):

- **`DATETIME` is returned as `"2026-08-03 18:34:37"`**, not
  `"2026-08-03T18:34:37.000Z"`. The console renders both identically.
- **`limit` is clamped to 1000.** This service put the value straight into
  `LIMIT` with no ceiling.

And one thing was added: migrations now run **automatically on start**.

## The migrations moved

`logservice-go/migrations/` holds byte-identical copies of every file in
`migrations/` here, under the same names, and CI asserts they still match.
`_migrations` keys on filename, so a database migrated by this service is seen as
fully migrated by the Go one.

**Write new migrations in `logservice-go/migrations/` only.** A file added here
would not be applied by anything the stack runs.

## Running it anyway

Unchanged, and still a standalone Bun package with its own `bun.lock`
(deliberately outside the pnpm workspace):

```bash
cd legacy/logservice
bun install
bun run start            # src/index.ts
bun run start:migrate    # migrate on boot, then serve
bun run db:migrate       # apply pending migrations and exit
bun test tests/          # still wired into `pnpm test`
```

Prefer changing `logservice-go/`. Touch this only to keep it building.
