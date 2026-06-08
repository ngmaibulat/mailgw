# Logservice Bun Rewrite Plan

## Stack Changes

| Current | Bun equivalent |
|---|---|
| Express | `Bun.serve` + native router |
| Sequelize ORM (12 models) | Raw SQL via Bun's SQL client |
| Sequelize migrations (12 files) | Plain SQL migration scripts |
| dotenv | Native (`Bun.env`) |
| Zod | Keep as-is (framework-agnostic) |

## Effort by Piece

- `src/index.js` + routes (3 endpoints) — ~2h, straightforward
- `src/validation/delivery.js` — 0h, no change needed
- `config/config.js` — ~30min
- `src/functions.js` query builder — **3-5h** — hardest part; the dynamic filter builder with Sequelize operators needs to be rebuilt as parameterized SQL
- 12 Sequelize models — ~3-4h for the ones actually in use (`/connection` and `/queue` don't actually persist — those writes are commented out)
- 12 migrations — ~1-2h to convert to plain SQL files with a simple runner

## Total Estimate

**1.5–2.5 days** for a careful rewrite at feature parity.

The existing logservice is small. The only genuinely complex piece to port is the Sequelize query builder abstraction in `src/functions.js`.
