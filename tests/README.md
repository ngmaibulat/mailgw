# End-to-end tests

Cross-cutting e2e tests for the whole mailgw stack. This is a standalone Bun
package (its own `package.json`/`tsconfig.json`), intentionally **not** a pnpm
workspace member — same as the rest of the Bun-based code here, so pnpm and Bun
don't fight over `node_modules`.

```
tests/
  harness/       the tier-B harness: a real gateway process, a scriptable
                 relay, a fake logservice, and a typed control-API client
  gw/            TIER B — spawns cmd/mailgw-go-test. No Docker, no network.
  stack/         TIER A — the compose stack: the real image, the console,
                 MariaDB, logservice, MailHog
  api/           logservice HTTP API e2e     (logservice.e2e.test.ts)
  smtp/          SMTP contract against the running gateway
  fixtures/      dev-profiles.json, generated from stack/baseline.ts
  provision.ts   drives the console to configure the gateway
```

## Two tiers

**Tier B (`gw/`) needs only a Go toolchain.** It builds `cmd/mailgw-go-test`,
runs it as a real process on a throwaway data directory, and points it at a fake
relay it can break and repair on demand. This is where the delivery path lives:
deferral, retry, quarantine, bounces, and a restart over one data directory.

```bash
pnpm test:e2e:gateway          # builds the binary, then runs tests/gw
```

**Tier A (`stack/`) needs the compose stack**, and owns what fakes cannot prove:
that MailHog — a third-party MTA — accepts what the gateway produces, that the
audit rows survive the real logservice, and that the bundle the **console**
composes is one the gateway accepts.

```bash
pnpm stack:test                # up with the overlay, then provision
pnpm test:e2e:stack
```

The rule for which tier a test belongs to — and why most things belong in Go
rather than either — is in `docs/internal/dev/testing.md`.

::: warning `bun test tests/` is a filter, not a path
It matches every directory named `tests` in the repo, including
`legacy/logservice/tests/`. Use `bun test ./tests/...`; the package scripts do.
:::

All of these talk to a **running** stack except tier B, so bring it up first:

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
approve its fingerprint; assign and deploy; then wait until it is **ready**.

```bash
pnpm provision          # or: bun tests/provision.ts
```

### "Ready" has one definition, and it is not a TCP connect

`tests/stack/ready.ts` owns it, and `provision.ts` and every Tier-A file gate on
it: **approved, holding a console-issued configuration, SMTP bound, and
answering 220 on it.**

That file exists because the weaker version of the question was being asked.
Provisioning used to open a TCP connection to `127.0.0.1:2525` and call the
stack ready — but compose publishes that port from the moment the *container*
starts, so with Docker's userland proxy the connect succeeds whether or not the
gateway process has bound anything, and nothing read the 220. Provisioning
therefore reported success roughly a millisecond after the deploy, and the tests
raced a gateway that was still `pending`. The gateway needs real time here by
design: its poll loop waits a jittered 15s before its first `/agent/status` and
registration does not wake it.

Two routes, so it works without the engineering image too: the control API on
9090 when it answers (it is the only source that can tell a console-issued
version from an injected one, and the only one that reports which addresses
actually bound), otherwise the gateway's own `/readyz` on `:8080`, which is open
because the dev stack deploys no `admin.metrics_token`. **Both** then have to
pass the SMTP greeting check — `/readyz` reads `serving`, which is set before
the listeners bind. A failure names the reason rather than surfacing three files
later as `connection closed by peer`.

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
pnpm test:e2e:gateway      # tier B only (no stack needed)
pnpm test:e2e:stack        # provision, then tier A only
pnpm test:e2e:smtp         # provision, then the SMTP contract
bun test ./tests/          # everything, assuming the stack is provisioned
bun test ./tests/api/      # logservice API only (needs no gateway)
```

Or from inside `tests/`: `bun run test`, `test:api`, `test:smtp`, `test:gw`,
`test:stack`, and `bun run typecheck`.

## Opt-in suites

The tests that mutate the database are **skipped by default** and enabled with
an env flag:

| flag | suite | what it does |
|---|---|---|
| `MAILGW_API_E2E=1` | `api/logservice.e2e.test.ts` | POSTs events to the logservice and reads them back via the search API |
| `MAILGW_DB_CHECK=1` | `smtp/tests/smtp.e2e.test.ts`  | sends real mail, then confirms rows landed in the DB |

**The `pnpm test:e2e*` scripts set these.** Nothing did before, so
`pnpm test:e2e:api` ran zero tests and reported success. `tests/stack/` is
deliberately not behind a flag: it needs no more than the stack the rest of the
suite already needs.

```bash
MAILGW_API_E2E=1 bun test ./tests/api/
MAILGW_DB_CHECK=1 bun test ./tests/smtp/
```

Other variables worth knowing:

| var | meaning |
|---|---|
| `MAILGW_GO_TEST_BIN` | use this binary for tier B instead of building one |
| `MAILGW_REQUIRE_TIER_B=1` | make a missing Go toolchain a failure, not a skip (CI sets it) |
| `MAILGW_KEEP_DATA=1` | keep tier-B data directories after a run, for debugging |
| `MAILGW_REQUIRE_TIER_A=1` | make an unusable stack a failure, not a skip (CI sets it) |

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
| `GATEWAY_ADMIN_URL` | `http://127.0.0.1:8080` | the gateway's own admin listener, where `/readyz` is the fallback readiness signal |
| `GATEWAY_CENTRAL_URL` | `https://webui:4000` | the console **as the gateway sees it**, for enroll |
| `CONSOLE_EMAIL` / `CONSOLE_PASSWORD` | `admin@lab.example` / `labpassword1` | the first admin it creates and signs in as |
| `SMTP_PORT` | `2525` | the port it waits for the gateway to answer 220 on |
| `RELAY_HOST` / `RELAY_PORT` | `mailhog` / `1025` | where deployed mail is relayed |

SMTP settings (`SMTP_HOST`/`_PORT`/`_FROM`/`_TO`) and the SMTP-e2e DB settings
(`MAILGW_DB_*`) are documented in [`smtp/README.md`](smtp/README.md).
