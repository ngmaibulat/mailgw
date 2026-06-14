# logservice-bun TODO

## Setup
- [x] Create `package.json` with Bun as runtime
- [x] Add `bun:sql` MariaDB connection config (replace `logservice/config/config.js`)
- [x] Set up environment variables via `Bun.env` (remove `dotenv` dependency)

## Server & Routing
- [x] Create `src/index.ts` with `Bun.serve` (replace Express `src/index.js`)
- [x] Implement native Bun router (replace `express.Router`)
- [x] Port `src/routes/root.js` → `src/routes/root.ts` (`GET /`)
- [x] Port `src/routes/api.js` → `src/routes/api.ts` (3 endpoints below)
  - [x] `POST /api/delivery` — validate + persist
  - [x] `POST /api/queue` — currently just logs, wire up persistence
  - [x] `POST /api/connection` — currently just logs, wire up persistence

## Validation
- [x] Copy `src/validation/delivery.js` → `src/validation/delivery.ts` (Zod, no changes needed)
- [x] Fix: use `parsed.data` instead of `req.body` when calling DB insert (bug in original)

## Database
- [x] Create `src/db.ts` — `bun:sql` MariaDB connection singleton
- [ ] Replace Sequelize models with typed SQL queries for active tables:
  - [x] `Delivery`
  - [x] `Connection`
  - [x] `Transaction`
  - [x] `MailAddr` + `LinkAddrTransaction` join
  - [x] `BlockMD5` / `HashLookup` (used in `functions.js` hash check logic)
  - [ ] Remaining models (`Header`, `Log`, `Config`, `RelayGroup`, `Relay`, `User`, `Exception`) — port only if actively used

## Query Builder
- [x] Port `src/functions.js` `createQuery` / `getData` to parameterized SQL
  - [x] `begins` → `LIKE 'val%'`
  - [x] `contains` → `LIKE '%val%'`
  - [x] `ends` → `LIKE '%val'`
  - [x] `is` / `=` → `= ?`
  - [x] `between` → `BETWEEN ? AND ?`
  - [x] `>`, `>=`, `<`, `<=`, `less`, `more` → direct operators
  - [x] Add field name allowlist to prevent column injection (security fix)
- [x] Port `checkMD5`, `hashLookup`, `hashListLookup` functions
  (as `decideActions` + `hashListLookup` in `src/query/hash.ts`, exposed via
  the `POST /filter/md5` route)

## Migrations
- [x] Convert 12 Sequelize migration files to plain `.sql` files
- [x] Write a simple Bun migration runner script

## Security Fixes (carry over from original)
- [x] Add authentication to API endpoints (API key via X-API-Key header)

## Testing
- [ ] Smoke test all 3 API endpoints (requires live DB)
- [ ] Verify DB writes succeed for `/delivery` (requires live DB)
- [x] Verify delivery validation rejects bad payloads
- [x] Test query builder search operators
- [x] Test auth middleware
