# M3 — Central Management server (`webui-fastify`)

**Status:** done  ·  **Packages:** `webui-fastify`, `logservice/migrations`  ·  **Depends on:** —  ·  **Blocks:** M4, M5, M6

> Migrated verbatim from `mailgw-go/TODO.md` on 2026-07-29.
>
> The **contracts** this milestone fixed — signing, fingerprints, bundle shape —
> are written up in detail in [README.md](./README.md), which M4 and M5
> implement the other side of. Read that first; this file is the record of what
> was built and what was left open.

## Goal

The console side of central management, built before any Go changes so the
gateway has something to register with. A gateway stops owning its
configuration: it registers, an operator approves its fingerprint, and
everything after that is pulled from central.

## Delivered

- [x] Schema: `logservice/migrations/016`–`021` — `Gateways`, `ConfigProfiles`,
      `GatewayAssignments`, `ConfigVersions` + `ConfigDeployments`, `Users.role`,
      `Sessions`. Mirrored describe-only in `webui-fastify/db/schema.ts`.
- [x] Agent API at the root Fastify scope (`/agent/*`) — outside the cookie gate
      *and* outside the audit-log hook, so a polling fleet cannot flood `Logs`.
      `POST /register`, `GET /status`, `GET /config`, `POST /report`.
- [x] **Ed25519 request signing, no tokens.** A gateway generates its own
      keypair; registration is open and lands `pending`; an operator approves a
      *fingerprint*. Canonical string is
      `METHOD\nurl\nunix-seconds\nsha256(body)`, 300s skew, scope-local raw-body
      parser so the signature covers the exact bytes sent.
- [x] Bundle composition (`src/central/bundle.ts`): assigned profiles + relay
      groups → one JSON document whose keys mirror the config-directory files
      (`server.yaml`, `routing.yaml`, `ngmfilter.json`, `relays.json`,
      `logging.json`). Handles the shape mismatches in one place — group name is
      the top-level key, `Relays.host` → `exchange`, port INT → number.
- [x] Deploy freezes an immutable `ConfigVersions` row; rollback repoints
      `desired_version_id` at an older one rather than minting a new bundle, so
      what runs afterwards is byte-identical to what ran before. Redeploying an
      unchanged configuration re-points instead of piling up versions.
- [x] Console: `/gateways` inventory + detail (approve/reject/revoke, rename,
      assignments, deploy, version list with rollback, deploy history),
      `/config/profiles` CRUD. The `/config/routing` `notimpl` stub is gone —
      routing is now a `ruleset` profile carrying mailgw-go's `routing.yaml`.
- [x] Prerequisites that genuinely blocked a fleet console: **persistent
      sessions** (a `Sessions` table, so a restart no longer logs every operator
      out and >1 replica works) and **roles** (`requireAdmin` on approval and
      every config mutation — previously any logged-in user could approve a
      gateway and read relay credentials).

## Deliberately not done, and why

- **Validating the rule DSL in the webui.** The compiler is Go. A second
  implementation in TypeScript would be a second source of truth that can
  disagree with the gateway. The console does shape checks only; the gateway
  validates on pull, keeps its last-good config, and reports `apply_error`.
- **Encrypting `auth_pass` at rest.** Still plaintext in `Relays`. Now that the
  bundle is a real consumer this is finally designable, but it belongs with M5
  (the decrypting side) rather than ahead of it.

## Known gaps

- [ ] A signed request can be replayed inside the 300s window. The signed routes
      are idempotent reads plus a metrics report, so the worst case is a stale
      report being reapplied — a nonce table would close it if reports ever
      drive anything but display.
- [ ] `GATEWAY_LOGSERVICE_URL` is one fleet-wide value. A multi-site fleet where
      gateways reach logservice at different URLs would want it per-profile.

## Defects found after the fact

From the review of 2026-07-29 — see
[M9](./M9-correctness-and-durability-fixes.md):

- **[M9.4] No DB transactions** around any multi-statement mutation.
  `saveAssignments` deletes every assignment then re-inserts one at a time, so a
  mid-sequence failure leaves the gateway with none — and the next Deploy
  freezes that empty state as a real, immutable, rollback-able version.
- Not scheduled, but recorded: `/agent/register` has no rate limit, so
  unlimited `pending` rows can be minted by anything that can reach the console;
  `POST /agent/report` trusts `applied_version_id`; and `bundle.ts`'s
  `bodyOfKind` silently picks the first assignment of a kind.
