# logservice

A Bun HTTP service (`Bun.serve`) over MariaDB. Receives audit events from
gateways and serves searches to the console.

```bash
cd logservice
bun run dev              # bun --watch
bun run start            # src/index.ts
bun run start:migrate    # migrate on boot, then serve
bun run db:migrate       # apply pending migrations and exit
bun run db:reset         # drop everything, then re-migrate
bun test tests/
```

## It owns the schema

**This is the only thing that migrates the database.** The console describes the
tables it queries in `db/schema.ts` but does not own them, and there is no
`drizzle-kit` there for that reason.

Adding a column means: write `logservice/migrations/NNN_*.sql`, then describe it
in the console's `db/schema.ts`, then use it.

## Migrations

Plain numbered `.sql` files in `logservice/migrations/`, applied in order by
`src/dbmigrate.ts`, which tracks applied filenames in a `_migrations` table.

Behaviour that constrains how you write one:

- Files are sorted **lexicographically**, so keep the three-digit prefix.
- The whole file is executed as one string, so several statements per file is
  fine.
- **A failed file is not recorded** — it throws and aborts. There is no down
  migration and no per-statement transaction, so write idempotent DDL
  (`CREATE TABLE IF NOT EXISTS`) where you can.

::: tip Migrations here are essay-commented, and that is the convention
Every migration from 017 onward opens with a comment block explaining why the
column exists and what it is *not*. Read `025_add_relay_use_mx.sql` or
`026_create_credential_sets.sql` before writing a new one.
:::

No foreign keys anywhere — referential integrity is enforced in the application.
Booleans are `TINYINT(1) NOT NULL DEFAULT 0`. `createdAt`/`updatedAt` are
`DATETIME NOT NULL` with **no** database default; the application fills them.

## Routes

| Route | Purpose |
|---|---|
| `GET /` | health check |
| `POST /api/connection` | inbound connection events |
| `POST /api/queue` | queue events, stored as `Transaction` rows |
| `POST /api/delivery` | delivery events, validated with Zod |
| `GET /api/{connection,delivery,transaction,hashlookup}` | search |
| `POST /filter/md5` | attachment blocklist check |

Each handler is wrapped by `handle()` = `withAuth(withErrorHandling(...))`. Auth
checks `X-API-Key` against `API_KEY`; **when `API_KEY` is unset, every request is
accepted.**

## Search

`GET` endpoints take a JSON `q` parameter:

```json
{ "search": [{"field": "…", "operator": "…", "value": "…"}],
  "searchLogic": "AND", "sort": [{"field": "id", "direction": "DESC"}],
  "limit": 50, "offset": 0 }
```

Fields are checked against per-table allowlists in `src/query/search.ts`.

::: danger buildWhere silently skips a field it does not recognise
So a column added to a table and to a grid but forgotten in the allowlist yields
a filter that appears to work and **returns every row**. The three allowlists are
exported precisely so a unit test can assert membership without a database. Add
to them in the same commit as the column.
:::

Each search returns the real `total` matching-row count — a `COUNT(*)` over the
same `WHERE` — not just the page size, so clients can paginate.

## Data access

Raw SQL through Bun's native `Bun.SQL` (MySQL adapter). **There is no ORM.**
Per-table helpers live in `src/models/`.

::: warning Bun supports MariaDB natively
Import `SQL` from `"bun"` and pass `adapter: "mysql"`. Without the adapter it
defaults to Postgres and hangs. Do not add `mysql2` here.
:::

## What the log rows carry

Every row has a `gateway` column naming which box wrote it, and a `Delivery` row
also has `route_rule` — the rule that sent that recipient there. Both are
optional on the wire and NULL for anything written before they existed.

`route_rule` is **per recipient**, not per envelope: one envelope groups by relay
group and can hold recipients routed there by different rules. That is why there
is no `route_rule` on `Transaction`.
