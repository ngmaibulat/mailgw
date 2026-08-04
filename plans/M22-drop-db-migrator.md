# M22 — Dropping `db-migrator`: the console waits for the schema itself

**Status:** **done** (2026-08-04)  ·  **Packages:** `webui-fastify`, `logservice-go` (comments only), `docker-compose.yaml`, `deploy/core`, `docs`  ·  **Depends on:** M21  ·  **Blocks:** —

> A subtraction, not a feature. One container leaves both compose files and
> nothing replaces it in either. The milestone is judged by one question:
> **does `docker compose down -v && docker compose up -d && pnpm provision`
> still bring up a working stack from an empty volume?**

## Why

M21 gave logservice a `serve` that migrates before it binds
(`logservice-go/cmd/logservice/serve.go`): `migrate.Check` → `migrateOnStart` →
serving pool → `checkFields` → `MarkReady()` → **`ListenAndServe` last**, with a
failed migration fatal. From that moment the `db-migrator` container was
applying migrations that `logservice` would have applied itself thirty seconds
later.

It survived anyway, for one line — `webui`'s

```yaml
depends_on:
    db-migrator:
        condition: service_completed_successfully
```

The console reads fifteen tables it does not own and, unlike every other
service, **never talks to logservice at boot**, so nothing else in the stack
could tell it the schema existed. The second gate (`logservice` on
`db-migrator`) was pure ordering: two processes racing to apply the same file
both pass the "already applied?" check, one loses on `_migrations.name` UNIQUE,
exits 1, and — because dependents gate on `service_completed_successfully` —
takes the stack with it. That race was noted in `notes/review-2026-07-29.md` and
is the second thing this milestone deletes, by leaving exactly one process that
migrates.

## What was decided, and against what

**The console waits for the schema itself.** At boot, after the existing
connection ping, `webui-fastify` polls `information_schema.TABLES` until every
table `db/schema.ts` declares exists, then serves.

The alternative on the table was a `logservice healthcheck` subcommand that
self-dials `/readyz`, a compose `healthcheck:` on the logservice service, and
`webui: {condition: service_healthy}` — which `logservice-go/README.md` had
already prescribed the shape of, since the distroless image has no shell for a
`curl` test. It is a good design and it is **not** what this does. Two reasons
to prefer the app-side wait: the gate then holds wherever the console runs,
including a bare `node src/index.ts` against a fresh database with no compose
file anywhere near it; and it makes the console's dependency **its own
statement** rather than a fact about a sibling service's readiness endpoint.
The healthcheck section in `logservice-go/README.md` stays as written — it is
still true, and it is now the road not taken.

**The expected table list is derived, not hardcoded.** `db/index.ts` already
does `import * as schema from "./schema.ts"`; `getTableName` + `is(v, Table)`
from drizzle-orm turn that into the list of tables this build queries. A
hardcoded `"Users"` would have been shorter and would have gone stale the first
time a milestone added a table — M13 added two, M6 added one. It also means a
console deployed ahead of its migration says *which* table it is waiting for
instead of answering 500 at runtime.

**`logservice migrate` stays.** Nothing in compose runs it any more, which is
usually the argument for deleting a subcommand — M18 made exactly that argument
about the gateway's second configuration source. It does not apply here, because
`migrate` is not a second way to configure anything: it is the same
`migrate.Run` that `serve` calls, without binding a port. It keeps two jobs:
`deploy/core/upgrade.sh` runs it **before** recreating services, so a bad
migration aborts the script with the old stack still serving; and it is the
foreground escape hatch when `serve` is crash-looping on that same bad
migration, where the alternative is reading a restart loop's logs.

## What changes

- **`webui-fastify/db/index.ts`** — `expectedTables()` and `waitForSchema()`
  beside `assertDbConnection()`. Polls every second up to
  `SCHEMA_WAIT_TIMEOUT_MS` (default 90 000; `0` disables), logs nothing when the
  schema is already there, and on timeout prints the missing tables and exits 1.
- **`webui-fastify/src/index.ts`** — calls it after `assertDbConnection()` and
  before `countUsers()`, which until now swallowed a missing `Users` table into
  a `User count check failed` warning.
- **`docker-compose.yaml`, `deploy/core/docker-compose.yaml`** — the
  `db-migrator` service is deleted; `logservice` takes over its `mariadb` edge;
  `webui` depends on `[mariadb, logservice]` for ordering only.
- **`deploy/core/upgrade.sh`** — `docker compose run --rm logservice migrate`.
- **`deploy/core/deploy.sh`** — the stale ordering comment, and `--retry` on the
  smoke curls, which no longer have compose blocking ahead of them.
- **`logservice-go`** — six comments that justified themselves by a container
  that no longer exists. No behaviour change; the module is untouched otherwise.

`mailgw-go` keeps `depends_on: logservice: service_started` deliberately. Gating
SMTP availability on the audit service would turn a slow database into delayed
mail; `internal/events` retries and then spills to disk precisely so it does
not have to.

## The trade this makes

A fatal migration used to stop `docker compose up` with one error, and nothing
else started. Now `logservice` restart-loops on it under `restart:
unless-stopped` while the console independently times out after 90 s and
restarts too. The stack is down either way; it is noisier and slower to read.
That is what the explicit `run --rm logservice migrate` in `upgrade.sh` buys
back on the one path where a human is watching.

`_migrations.name` UNIQUE stays, and its comment is rewritten rather than
removed: it no longer guards "the migrator against logservice" — nothing races
in the shipped stack now — but it still guards a scaled service and an upgrade
that briefly overlaps an old and a new container.

## The same change in `mailgw-deploy`

The lab stacks live in a separate repo and carried their own copy of the
service, as `lab-db-migrator` in labs 01 and 02. They were changed to match:
the service deleted, `lab-logservice` given the `lab-mariadb` edge it inherited,
`lab-webui` left with start-ordering only. The Ansible `compose_up` role used to
block on `lab-db-migrator exited 0` before the play went on to touch the
console; it now waits for the console's own `webui listening` line, which is the
better signal anyway — `docker compose ps` says "running" the whole time it is
waiting for the schema.

**`labs/bin/` is gone with it**, and that is the larger half of the change.
There are two deployment paths and nothing in between: `docker compose up -d`,
or Ansible. Everything else is configuration, and configuration is done in the
console. Each script was deleted by making the thing it wrapped do its own job:

| Was | Now |
|---|---|
| `lab-net.sh` created the shared network | every lab declares it — `name: mailgw-lab`, pinned `172.30.0.0/24` — so whichever comes up first creates it |
| `gen-certs.sh` made the console's TLS pair | the console mints a self-signed pair when its directory is empty |
| `claim-code.sh` read a gateway's claim code | `docker exec lab-gw mailgw-go claim status`, which is what it shelled out to |
| `lab-db-migrator` | logservice migrates on start, before it binds |

The network had to stop being `external: true` for that to work, and the
sequencing is the interesting part: a network declared with `name:` and no
`external: true` **refuses** to attach to one docker created by hand ("was found
but has incorrect label"), so compose-managed and `lab-net.sh` cannot coexist —
deleting the script is what makes the declaration possible, not a consequence of
it. Two compose projects declaring the same named network do share it, which is
what labs 02 + 03 need. Hosts that ran the old script need `docker network rm
mailgw-lab` once, and every lab file says so.

The console self-signing (`webui-fastify/src/tls.ts`, `ensureTlsPair`) is the
same decision M8 made for the gateway — a node expected to come up unattended
cannot also require an operator-supplied file — and it removes the cert step
from the dev stack and `deploy/core` too, not just the labs. **A pair that is
already there is never touched**, which is the whole contract and what keeps a
real certificate on a real deployment; half a pair is refused rather than
half-replaced. It costs one dependency, `node-forge`, already used by the repo's
own `certs/` project. The cert mounts lost their `:ro`, or generating would be
the crash it replaced.

Alongside: **defaults.** `DB_NAME`, `DB_USER` and `CORE_HOST` default in both
repos' compose files (`mailgw`, `mailgw`, `lab-logservice`/`localhost`), and
`SIGN_COOKIE` defaults in the labs to the value `.env.example` already ships.
The **passwords do not** — `DB_PASS`, `DB_ROOT_PASS`, plus `API_KEY` and
`SIGN_COOKIE` on the production core node — because a compose file that invents
a password is a compose file whose password reaches production, and a
defaulted-empty `API_KEY` would silently switch off authentication on the
endpoint every edge node posts to.

The Ansible side shrank to match: `roles/lab_files` no longer creates the
network or generates certificates (its hand-made network is precisely what
compose would now refuse), and `roles/compose_up` waits for the console's own
`webui listening` line instead of a migrator container exiting 0.

## Ordering hazard

`deploy/core/` runs `mailgw-webui-fastify:latest`. An **old** webui image plus
the new compose file is the one bad combination: the compose gate is gone and
the image does not yet carry the wait, so a fresh install could start the
console against an empty schema. Push the webui image before or with the compose
change; `docker compose pull` is already first in both deploy scripts.

## Verification

```bash
pnpm check
pnpm --filter mailgw-webui-fastify typecheck && pnpm --filter mailgw-webui-fastify check
pnpm --filter mailgw-webui-fastify test
docker compose config --services            # no db-migrator
docker compose down -v && docker compose up -d
```

Then: no `dev-mailgw-db-migrator` container exists; `docker compose logs webui`
shows the wait and then `DB Connection: OK` with no `User count check failed`;
`SELECT COUNT(*) FROM _migrations` is 26. The negative case is worth running
once — bring up mariadb and webui with logservice stopped, watch the console
name its missing tables and exit after 90 s, then start logservice and watch it
recover unattended.

### What was built differently, and what verification found

- **`waitForSchema` sanitises its own environment variable.** `Number("soon")`
  is `NaN`, and a `NaN` deadline is not a long wait — it is a boot that never
  finishes, since every comparison against it is false. A malformed
  `SCHEMA_WAIT_TIMEOUT_MS` warns and falls back to 90 s, the rule
  logservice-go's `envInt` already follows. Only an explicit `0` disables the
  wait.
- **`webui` needed `mariadb` added to its `depends_on`.** It had been reaching
  the database transitively through `db-migrator`; deleting that service would
  have left the console with no ordering edge to MariaDB at all.
- **The gate was verified by withholding logservice**, not by reading the code:
  `up -d mariadb` + `up -d --no-deps webui` produced
  `Schema: waiting for logservice to migrate 14 of 14 tables (…)`, `/login`
  answering nothing, and then `Schema: ready after 10s` plus a `302` the moment
  logservice was started — recovery with no restart and no intervention.
- **A verification detour worth recording:** `pnpm provision` run repeatedly
  fails with `sign-in did not yield a session cookie (status 302)`. That is the
  console's own login rate limit (`POST /login`, 5 per minute per IP,
  `LOGIN_RATE_MAX` / `LOGIN_RATE_WINDOW` in `src/app.ts`) and has nothing to do
  with this milestone — but the error message names credentials, not
  throttling, so it reads like a broken stack. Space provisioning runs out, or
  raise the limit for the run.

`pnpm stack:test` and `pnpm test:e2e:api` must pass **unedited**: no
`package.json` script and no CI step names a service, and
`tests/stack/console.ts` already retries the console for 120 s. If either needed
an edit, the gate is in the wrong place.
