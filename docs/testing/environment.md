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

## Two modes, and which plans need which

| Plan | File mode | Managed |
|---|---|---|
| TP-01 … TP-07 | yes | yes |
| TP-08, TP-09 | — | **managed only** |
| TP-10 | yes | yes |

TP-08 and TP-09 test provisioning and versioned deployment, which only exist in
managed mode.

::: warning The development compose pins the gateway to file mode
`docker-compose.yaml` passes `-config /opt/mailgw-go/config` deliberately: a
managed gateway does not serve SMTP until an operator has approved it and
deployed a configuration, so `pnpm test:e2e:smtp:go` against a fresh managed
stack would find nothing listening.

In that configuration the admin UI is a **read-only status page** — there is no
claim code and `POST /register` answers `404`, so **TP-08 and TP-09 cannot be run
against it as shipped.** Delete the `command:` line from the `mailgw-go` service
and recreate the container; it then boots empty and waits at
`http://localhost:8080`.
:::

For file-mode plans, run the gateway against a configuration directory you can
edit:

```bash
cp -r mailgw-go/testdata/config /tmp/tp-config
# edit /tmp/tp-config/server.yaml so outbound.spool_dir points somewhere writable
cd mailgw-go && go run ./cmd/mailgw-go serve -config /tmp/tp-config
```

::: warning The default spool directory is not writable
`outbound.spool_dir` defaults to `/opt/mailgw-go/queue`. Change it before you
start, or the gateway exits with `cannot open spool` and every plan is blocked at
step 1.
:::

## Resetting between plans

```bash
docker compose down -v && docker compose up -d     # everything, including the database
rm -rf /tmp/tp-config/../queue/*                   # just the spool
```

A plan that leaves mail in the queue affects the next one. The cleanup section of
each plan tells you what it left behind.

## Reading the gateway's log

```bash
docker compose logs -f mailgw-go
```

JSON by default. Several plans ask you to check for a specific warning, and those
are the exact strings to search for.
