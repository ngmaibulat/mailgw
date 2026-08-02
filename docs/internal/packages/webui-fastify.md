# webui-fastify

The admin console. Fastify over native HTTP/2, TypeScript run directly by Node
with no build step.

```bash
pnpm webui:dev                                  # nodemon
pnpm --filter mailgw-webui-fastify typecheck    # tsc --noEmit
pnpm --filter mailgw-webui-fastify check        # biome lint + format check
pnpm --filter mailgw-webui-fastify check:fix
pnpm --filter mailgw-webui-fastify test         # node --test
```

::: warning It will not start without TLS certificates
It reads `./certs/server.{key,crt}` relative to its working directory at boot.
Run `pnpm certs` first. Locally a `certs` symlink points at
`certs/generated/webui`; in Docker the same directory is mounted.
:::

## No build step

TypeScript run directly by Node's native type-stripping. `tsconfig.json` exists
for IntelliSense and `pnpm typecheck` only, and `typescript` is a devDependency
the production image omits.

Consequences you will hit immediately:

- `verbatimModuleSyntax` + `erasableSyntaxOnly`, so relative imports carry the
  real `.ts` extension: `import { build } from "./app.ts"`.
- No enums, no parameter properties, no decorators.

## Encapsulation is the auth boundary

Three nested scopes, and this replaces Express's ordering-dependent `app.use()`
chain:

1. **root** — static assets. Public and unlogged.
2. **logged** — adds the audit-log `onRequest` hook, plus `/login`, `/logout`,
   `/profile`. Ungated.
3. **secured** — adds the `checkSession` `preHandler`, then every protected route.

Hooks apply only to their own scope and its descendants, so static stays unlogged
and the auth routes stay ungated by construction rather than by ordering.

**`/agent/*` is a fourth sibling at the root scope**, deliberately outside both —
a gateway must not be redirected to `/login`, and a polling fleet would flood the
audit table. It brings its own Ed25519 signature `preHandler`.

## The audit log skips API reads

`isNoise()` intentionally skips `/favicon.ico` and high-frequency `GET /api/*`
grid reads. The log viewers page against those endpoints, and auditing them would
flood the table for no audit value. Page navigation and `POST /config/*` are
recorded.

This is separate from Fastify's pino request logging, which is enabled only when
`NODE_ENV != "production"`.

## Data

**Drizzle ORM**, scoped to only what the console owns — and it **does not own or
migrate the schema**. `db/schema.ts` describes columns to query; logservice's SQL
migrations create them.

`db/index.ts` exports a lazy `mysql2` pool with no import-time connect, plus
`assertDbConnection()` which `src/index.ts` pings at startup.

## Validation

`src/validation/config.ts` derives insert schemas from the Drizzle tables with
drizzle-zod, `.pick()`ed to form fields and `.extend()`ed to tighten them.

**`.pick()` is the mass-assignment defence** — zod strips everything else, so
`request.body` cannot set columns the form does not expose. The credential
schemas pick `username` and `set_id` but *not* `hash`, which is what stops a
crafted body writing a hash of its own choosing.

The whole application is on **zod v4**, imported from `"zod/v4"`.

## Roles

`Users.role` is `admin` or `viewer`. `requireAdmin` gates gateway approval,
deploy, rollback and configuration mutations.

::: warning The relay routes are not admin-gated, and look like an oversight
`/config/relay/*` and `/config/relaygrp/*` have no `adminOnly`, so any signed-in
viewer can create or edit a relay — including setting `auth_pass`. Meanwhile
`roles.ts` says the split exists precisely so a viewer cannot read relay
credentials, and gateway approval is gated because approving is what lets a
gateway pull them.

The credential-set routes added for inbound AUTH **are** all admin-gated,
including the index.
:::

## Read proxy

`/api/*` GETs validate the frontend's `?request=<json>` against a Zod schema,
forward it as logservice's `?q=<json>`, and pass the response through verbatim.
**The console does not query the log tables directly.**

Failures map to **502** (logservice answered non-2xx) or **504** (unreachable or
invalid JSON), not a blanket 500.

## Sessions

Database-backed (`Sessions` table). A restart no longer signs everyone out and
more than one replica works. `setSessionStore()` swaps in a `Map` so
`app.inject()` tests run without a database.

## Tests

`node --test`, no vitest or jest. The database is stubbed by monkey-patching the
Drizzle `db` object's `select`/`insert`/`update`/`delete` with a chainable stub
keyed by table identity.

Set `process.env.SIGN_COOKIE` **before** importing `./app.ts` — `checkenv.ts`
validates at import time.
