# End-to-end tests

Cross-cutting e2e tests for the whole mailgw stack. This is a standalone Bun
package (its own `package.json`/`tsconfig.json`), intentionally **not** a pnpm
workspace member — same as the rest of the Bun-based code here, so pnpm and Bun
don't fight over `node_modules`.

```
tests/
  api/           logservice HTTP API e2e     (logservice.e2e.test.ts)
  smtp/          SMTP client + pipeline e2e  (src/, tests/, swaks.sh)
  provision.ts   drives the console to configure the gateway
```

Both suites talk to a **running** stack, so bring it up first:

```bash
docker compose up -d
```

## The gateway must be provisioned first

**`docker compose up` no longer gives you a gateway that relays.** The gateway
takes its entire configuration from Central Management and has no other source —
no config directory, no environment, no flags — so a fresh stack has a gateway
that is deliberately not listening on SMTP: unclaimed, unregistered, unapproved
and holding no configuration.

`tests/provision.ts` walks it the rest of the way, exactly as an operator would
on a real edge node: create the first admin, a relay group pointing at mailhog
and the three config profiles; wait for the gateway to register itself and
approve its fingerprint; assign and deploy; then wait until it answers on SMTP.

```bash
pnpm provision          # or: bun tests/provision.ts
```

It is idempotent, so running it against an already-provisioned stack is a no-op.
The `pnpm test:e2e*` scripts run it for you; only the bare `bun test` forms below
skip it.

> This used to be unnecessary: the dev compose file pinned the gateway to file
> mode with a mounted config directory, so it relayed on `up`. That mode is gone.

### On a clean volume you need the engineering image

`pnpm provision` waits for the gateway to **register itself**, and against the
shipped image that only happens once somebody submits a Central Management URL
in its wizard on `:8080` — which since M12 is behind a session behind a claim
code printed to the container log. Nothing automates that step, so on a fresh
`mailgw_go_data` volume the wait times out after 120s.

You will not usually notice, because the volume is named: once the wizard has
been walked by hand, the identity and the URL survive every `docker compose
down` that omits `-v`. A fresh clone, a CI runner and `down -v` all hit it.

The engineering build closes it. `POST /testctl/enroll` calls the same
registration the wizard calls, and `provision.ts` probes for it:

```bash
pnpm build:mailgw-go:test    # build ngmaibulat/mailgw-go-test
pnpm stack:test              # up with the overlay, then provision
```

Nothing else about provisioning changes — the console still lands the gateway
pending, and the fingerprint approval, the assignment and the deploy are the
same. Against the shipped image the probe simply fails and the script waits as
before, so a hand-provisioned stack still works.

**The control API is unauthenticated.** It is bound to loopback in
`docker-compose.test.yaml`, never published, never tagged `:latest` and never
built by `pnpm docker:push`. See `mailgw-go/internal/testctl/doc.go`.

## Running

From the **repo root** (so Bun auto-loads the root `.env` for `PORT`/`DB_*`):

```bash
pnpm test:e2e              # provision, then everything
pnpm test:e2e:smtp         # provision, then SMTP only
bun test tests/            # everything, assuming the stack is already provisioned
bun test tests/api         # logservice API only (needs no gateway)
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

`provision.ts` reads:

| var | default | meaning |
|---|---|---|
| `CONSOLE_URL` | `https://localhost:4000` | the console to drive |
| `TESTCTL_URL` | `http://127.0.0.1:9090` | the engineering build's control API; probed, optional |
| `GATEWAY_CENTRAL_URL` | `https://webui:4000` | the console **as the gateway sees it**, for enroll |
| `CONSOLE_EMAIL` / `CONSOLE_PASSWORD` | `admin@lab.example` / `labpassword1` | the first admin it creates and signs in as |
| `SMTP_PORT` | `2525` | the port it waits for the gateway to answer on |
| `RELAY_HOST` / `RELAY_PORT` | `mailhog` / `1025` | where deployed mail is relayed |

SMTP settings (`SMTP_HOST`/`_PORT`/`_FROM`/`_TO`) and the SMTP-e2e DB settings
(`MAILGW_DB_*`) are documented in [`smtp/README.md`](smtp/README.md).
