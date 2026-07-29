# Backlog analysis — logservice & mailgw (Haraka) — 2026-07-29

Assessment of whether the **tracked** TODOs for `logservice/` and `mailgw/` reflect the
real state of those two packages. Companion to the full findings in
[`docs/review-2026-07-29.md`](docs/review-2026-07-29.md), which carries the `file:line`
detail for everything referenced here.

**Summary:** both tracked backlogs are thin and understate the actual work. Two of the
five open mailgw items are currently blocked or inert, and logservice's is a finished
migration checklist whose test plan points at a file that doesn't exist. The significant
work in both packages is not tracked anywhere.

---

## 1. What's tracked today

### `logservice/TODO.md` — effectively finished

It's a migration checklist (Express + Sequelize → Bun + `Bun.SQL`), and everything
substantive is ticked. Two unchecked lines remain:

- **Port the remaining models** (`Header`, `Log`, `Config`, `RelayGroup`, `Relay`,
  `User`, `Exception`) — qualified in the item itself with *"port only if actively
  used"*. They aren't: `webui-fastify` owns `users` / `relays` / `relayGroups` / `logs` /
  `exceptions` through its own Drizzle schema (`webui-fastify/db/schema.ts`), and
  logservice never touches them. **Closeable as obsolete.**
- **Testing** — the section defers entirely to `planning/testing.md`. That file **does not
  exist** (`logservice/planning/` is absent). So logservice's test plan is a dangling
  pointer.

That gap shows: there is no coverage of `src/query/search.ts`, `src/routes/api.ts`,
`src/middleware/error.ts`, `parseSearchQuery`, the fail-open auth path, or any model.

Two ticked items are also only half-true:

| Ticked item | Reality |
|---|---|
| `[x] Add field name allowlist to prevent column injection (security fix)` | Covered field names and sort direction, but **not the join operator** — that was the SQL injection fixed on 2026-07-29 |
| `[x] Add authentication to API endpoints (API key via X-API-Key header)` | Implemented, but **opt-in rather than enforced** — skipped entirely when `API_KEY` is unset |

### `mailgw/TODO.md` — 5 open items, all accurate, but half are unactionable

| Item | Status |
|---|---|
| `npFilterAttach`: stop hardcoding URLs | Real, but **inert** — `config/plugins` disables the plugin (as `#ngmFilterAttach`, which isn't even its filename) |
| De-duplicate event posting | Same — inert while `npFilterAttach` is off |
| `npConnection`: wire it up or clean it out | **Blocked** — the hook is unreachable regardless (§2) |
| Naming vs. behavior (`npData` posts connection info) | Cosmetic |
| Handle post failures | Real, and **understated** (§2) |

---

## 2. Significant, and not tracked

### mailgw

- **`npFilter.js:65` returns `next(OK)`, which ends the hook chain.** Haraka continues
  only on `CONT` (verified in `Haraka/plugins.js:464-470` — any other retval sets
  `hooks_to_run = []`). Since `config/plugins` lists `npFilter` first,
  **`npConnection.hook_connect` never runs in production.** Whatever is decided for the
  tracked `npConnection` item, it can't be tested until this is fixed. `hook_rcpt`
  (`:83`) does the same and blanket-accepts every recipient.

- **"Handle post failures" is worse than the TODO describes.** It's not merely
  fire-and-forget: `postWithLogging`'s error branch is **structurally unreachable** —
  `httplog` (`functions.js:28-30`) already terminates with `.catch()`, so it never
  rejects. Real connect failures instead fall through to the `else` at `:101` and are
  logged as *"HTTP Logfail, please review logs on Logger side"*, pointing operators at
  the wrong system. There is also **no `response.ok` check**, so an HTTP 500 from
  logservice is recorded as success. Net effect: dropped events look healthy.
  `tests/functions.test.js:243` passes only because it stubs `httplog` with a rejecting
  fake — it tests a path that cannot occur.

- **Duplicate `Connection` rows.** `npData.js:9` posts at `hook_data`, which fires per
  *transaction*, not per connection — a pipelined client produces N rows sharing one
  `connection.uuid`, and there's no unique key on it. This is a **different mechanism**
  from the tracked `npFilterAttach` overlap, and unlike that one it is live today.

- **`haraka-constants` is missing from `mailgw/package.json`.**
  `tests/npFilter.test.js:7` and `tests/npRoute.test.js:6` require it; it currently
  resolves only via a stale root `node_modules/` directory left over from an old
  `bun install`. A clean checkout plus `pnpm install` fails both files. Belongs in
  `devDependencies`.

- **Attachment scanning has a bypass and inverted failure modes** — `inline`-disposition
  attachments skip the MD5 blocklist entirely, network and MIME-parse errors fail *open*,
  but an empty response fails *closed*. Latent while the plugin is disabled; worth fixing
  **before** re-enabling rather than after.

### logservice

- **Auth fails open** when `API_KEY` is unset (`src/middleware/auth.ts:5-11`) — only a
  `console.warn`. `docker-compose.yaml` defaults it to empty while publishing port 3000.
  This is what made the SQL injection unauthenticated, and it remains a live issue on its
  own.

- **Three of four POST endpoints have no validation.** Only `/api/delivery` uses Zod.
  `/filter/md5` fails **open** on a malformed body (`{action:"allow"}`) — the attachment
  blocklist silently permits.

- **No secondary indexes anywhere** across all 14 migrations. `BlockMD5s.md5` is queried
  on every attachment of every message — a full table scan **on the SMTP hot path**.
  `Transaction.uuid` (the hashlookup JOIN key) is likewise unindexed.

- **`Delivery.response` and `rcpt_list` are `VARCHAR(255)`.** Under MariaDB's default
  strict mode, a long SMTP response or a multi-recipient list **errors**, turning a log
  write into a 500. Long responses and multi-recipient lists are routine.

- **Unbounded `limit`** (`src/query/search.ts:71`, no clamp) — `{"limit":100000000}`
  attempts to serialise the whole table.

---

## 3. Suggested next steps

For **mailgw**, in this order — the first unblocks a tracked item, the second makes
failures visible:

1. Fix the `next(OK)` returns in `npFilter` (unblocks the tracked `npConnection`
   decision).
2. Fix the unreachable error branch and add a `response.ok` check in
   `functions.postWithLogging` (upgrades the tracked "handle post failures" item into
   something that actually surfaces loss).
3. Add `haraka-constants` to `devDependencies` so a clean checkout passes.
4. Then revisit the two inert `npFilterAttach` items — decide whether that plugin is
   coming back at all, and fix the disposition bypass first if so.

For **logservice**:

1. Fail closed when `API_KEY` is unset.
2. Add the missing indexes — `BlockMD5s.md5` and `Transaction.uuid` first.
3. Widen `Delivery.response` / `rcpt_list` to `TEXT`.
4. Validate the remaining POST bodies and clamp `limit`.
5. Housekeeping: close the obsolete "remaining models" item, and either write
   `planning/testing.md` or point the Testing section somewhere real.

Neither backlog currently reflects any of this. Folding these items into the two
`TODO.md` files would make them usable again; that hasn't been done.
