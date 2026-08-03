# Test environment

## What you need

**A running stack.** Either the development compose file, or a core plus at least
one gateway.

```bash
pnpm certs          # the console will not boot without these
docker compose up -d
```

That gives you MariaDB, the log service, the gateway, the console, and
**MailHog** — an SMTP sink with a web interface, which is what makes these plans
runnable without sending real mail anywhere.

**A test client.** `swaks` is assumed throughout:

```bash
sudo apt install swaks          # Debian/Ubuntu
brew install swaks              # macOS
```

`openssl s_client` for the TLS plans, and `curl` for the endpoint plans.

## Addresses used

| What | Where |
|---|---|
| Gateway SMTP | `localhost:2525` (and host `:25`) |
| Gateway admin UI | `http://localhost:8080` |
| Console | `https://localhost:4000` (native HTTP/2 over TLS) |
| MailHog UI | `http://localhost:8025` |
| MailHog SMTP | `localhost:1025` |
| Log service | `http://localhost:3000` |

Check the compose file — these are the development defaults and your deployment
may differ.

## Every plan runs against a provisioned stack

There is one mode. A gateway takes its whole configuration from Central
Management — no environment, no arguments, no config files — so **every plan
starts from a stack you have provisioned**, and there is no shortcut past it.

```bash
pnpm certs                   # once: the console's TLS pair
docker compose up -d
pnpm provision               # profiles, approval, deploy — idempotent
```

`pnpm provision` is what makes the gateway relay: it creates the first admin, a
relay group pointing at MailHog and the ruleset/allowlist/server profiles, waits
for the gateway to register itself, approves its fingerprint, assigns and
deploys. Until it has run, the gateway is listening on `:8080` with a claim code
and **not** on SMTP — which is correct behaviour, not a broken stack.

::: tip This used to be two modes
Plans previously ran in "file mode" against a config directory
(`mailgw-go/testdata/config` copied to `/tmp/tp-config`), and `docker-compose.yaml`
pinned it. Both are gone. Where a plan says "edit `ngmfilter.json`", edit the
**`lab-allowlist` profile** in the console and press Deploy; the profile bodies
are the same text.
:::

**TP-08 and TP-09** test provisioning and versioned deployment. Run them against
a stack you have NOT yet provisioned — `docker compose down -v && docker compose
up -d`, then work through the wizard by hand instead of running `pnpm provision`.

## Changing configuration mid-plan

Every "edit the config and reload" step is now: edit the profile in the console,
press **Deploy**, and watch it land. It applies within a second (a WebSocket
notification; the 15s poll is the backstop). `SIGHUP` still works and means
"re-apply the cached bundle".

A configuration the gateway refuses to compile leaves the running one in force
and comes back as `apply_error` on the gateway's page — that is the intended
behaviour and several plans exercise it deliberately.

## Resetting between plans

```bash
docker compose down -v && docker compose up -d && pnpm provision
```

A plan that leaves mail in the queue affects the next one. The cleanup section of
each plan tells you what it left behind.

## Reading the gateway's log

```bash
docker compose logs -f mailgw-go
```

JSON by default. Several plans ask you to check for a specific warning, and those
are the exact strings to search for.
