# End-to-end tests

Cross-cutting e2e tests for the whole mailgw stack. This is a standalone Bun
package (its own `package.json`/`tsconfig.json`), intentionally **not** a pnpm
workspace member — same as the rest of the Bun-based code here, so pnpm and Bun
don't fight over `node_modules`.

```
tests/
  api/    logservice HTTP API e2e          (logservice.e2e.test.ts)
  smtp/   SMTP client + pipeline e2e       (src/, tests/, swaks.sh)
```

Both suites talk to a **running** stack, so bring it up first:

```bash
docker compose up -d
```

## Running

From the **repo root** (so Bun auto-loads the root `.env` for `PORT`/`DB_*`):

```bash
bun test tests/            # everything (opt-in suites stay skipped)
bun test tests/api         # logservice API only
bun test tests/smtp        # SMTP only
```

Or from inside `tests/`:

```bash
cd tests
bun test            # all
bun test api        # API only
bun test smtp       # SMTP only
```

There are also package scripts: `bun run test:api`, `bun run test:smtp`.

## Opt-in suites

The tests that mutate the database are **skipped by default** and enabled with
an env flag:

| flag | suite | what it does |
|---|---|---|
| `MAILGW_API_E2E=1` | `api/logservice.e2e.test.ts` | POSTs events to the logservice and reads them back via the search API |
| `MAILGW_DB_CHECK=1` | `smtp/tests/smtp.e2e.test.ts`  | sends real mail, then confirms rows landed in the DB |

```bash
MAILGW_API_E2E=1 bun test tests/api
MAILGW_DB_CHECK=1 bun test tests/smtp
```

## Configuration

The API e2e suite reads connection settings from the repo-root `.env`
(`PORT`, and — for completeness — the `DB_*` vars). Override per-run:

| var | default | meaning |
|---|---|---|
| `PORT` | `3000` | logservice port |
| `LOGSERVICE_URL` | `http://127.0.0.1:$PORT` | full base URL override |
| `API_KEY` | _unset_ | sent as `X-API-Key`; also enables the auth test |

SMTP settings (`SMTP_HOST`/`_PORT`/`_FROM`/`_TO`) and the SMTP-e2e DB settings
(`MAILGW_DB_*`) are documented in [`smtp/README.md`](smtp/README.md).
