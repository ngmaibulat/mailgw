# mailgw test client

Two ways to drive the running gateway:

- **`swaks.sh`** — the original shell/swaks one-shot send.
- **Bun-native client** — a zero-dependency SMTP client over `Bun.connect`
  (`src/smtp.ts`), with `bun:test` integration tests under `tests/` and a
  `src/send.ts` CLI. No nodemailer/mysql2 — just Bun's built-in TCP + SQL.

```
client/
  src/    smtp.ts (client), send.ts (CLI)
  tests/  smtp.test.ts (SMTP), smtp.e2e.test.ts (DB, opt-in)
  swaks.sh
```

All of it talks to a **running** mailgw, so bring the stack up first:

```bash
docker compose up -d
```

## Run the tests

```bash
cd client
bun test                      # SMTP-level integration tests
```

Override the target with env vars (defaults shown):

| var | default | meaning |
|---|---|---|
| `SMTP_HOST` | `127.0.0.1` | gateway host |
| `SMTP_PORT` | `25` | gateway port |
| `SMTP_FROM` | `me@ngm.dev` | envelope sender |
| `SMTP_TO` | `test@ngm.dev` | recipient (routable to a relay) |

`smtp.test.ts` covers: the 220 greeting, EHLO capabilities, a full
accept-and-queue transaction, and a couple of rejection cases. The
accept-and-queue test performs a **real relayed send** (like swaks).

## Full pipeline (opt-in, uses Bun's native SQL)

`smtp.e2e.test.ts` sends a message and then queries the logservice DB
(via `Bun.SQL`, `adapter: "mysql"`) to confirm a `Delivery` row landed. It is
skipped unless enabled:

```bash
MAILGW_DB_CHECK=1 bun test smtp.e2e
```

DB connection defaults to the compose dev stack; override with
`MAILGW_DB_HOST` / `_PORT` / `_USER` / `_PASS` / `_NAME`.

## One-off send

```bash
bun run src/send.ts                       # me@ngm.dev -> test@ngm.dev
bun run src/send.ts user@example.com sender@example.com
```

> Editor type hints: `bun install` (pulls `@types/bun`). Not required to run —
> `bun test`/`bun run` work without it.
