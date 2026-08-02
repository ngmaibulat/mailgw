# Installation

There are three ways to run mailgw, in increasing order of how much you have to
decide up front.

## Try it locally

The repository ships a working configuration directory. From a checkout:

```bash
cd mailgw-go
go run ./cmd/mailgw-go check -config ./testdata/config   # validate first
go run ./cmd/mailgw-go serve -config ./testdata/config   # listens on 2525
```

`check` exits non-zero on a bad configuration and prints what it understood —
which listeners, which relay groups, which rules, and any warnings. Run it before
`serve`, always; it is the cheapest test in the system.

::: warning The default spool directory
`outbound.spool_dir` defaults to `/opt/mailgw-go/queue`, which an unprivileged
user cannot create. For a local run, point it somewhere writable in your
`server.yaml`.
:::

Send it something:

```bash
swaks --server localhost:2525 --from you@example.com --to someone@ngm.dev
```

## The whole stack, with Docker

`docker-compose.yaml` at the repository root brings up everything: MariaDB, the
log service, the gateway, the admin console and a MailHog instance to catch
outbound mail.

```bash
pnpm certs          # generate the TLS pair the console needs
docker compose up
```

The gateway takes host port 25 and also publishes 2525. The console is on
HTTPS with a self-signed certificate.

## Production

Production is split by role, because the two halves scale differently and one of
them is internet-facing.

### The core — one per installation

MariaDB, the log service and the admin console.

```bash
cd deploy/core
cp .env.example .env        # then edit it
# drop a real TLS pair in deploy/core/certs/
bash deploy.sh
```

The setting most often got wrong is `CORE_HOST`: it is the address **edge
gateways** use to reach this host, so `localhost` is wrong unless everything runs
on one machine.

`CONFIG_SECRET_KEY` is optional and encrypts stored relay passwords. The console
decrypts them when it composes a configuration bundle, so the key never leaves
this host.

Upgrades:

```bash
bash upgrade.sh             # pull new images, re-run migrations
```

### An edge gateway — one per node

Numbered scripts, run in order, on a fresh Ubuntu host:

```bash
cd deploy/gateway
sudo bash 00-packages.sh
sudo bash 01-docker.sh
sudo bash 02-checkdirs.sh
sudo bash 03-cleanup.sh
sudo bash 04-gateway.sh     # takes no arguments
sudo bash 05-firewall.sh
```

`04-gateway.sh` pulls the image, starts the node, and prints two things you need:
the **wizard URL** and the **claim code**. The gateway does not relay anything
until an operator signs in with that code, registers it with the console,
approves its fingerprint and deploys a configuration.

::: danger 05-firewall.sh is required, not optional
The admin listener is always running, on a root process, on an internet-facing
host. It is authenticated — but what an authenticated caller can do is re-point
the gateway at a management console they control, which then hands it your relay
credentials. Narrowing who can reach the port is the second control, and you want
both.
:::

Back up **`/opt/mailgw-go/data`**. It holds the node's private key, its claim
code and its configuration cache. Lose it and the node is a stranger to the
console: it must be registered and approved again.

## Which mode am I in?

| | File mode | Managed |
|---|---|---|
| How it starts | `serve -config <dir>` | no arguments at all |
| Where configuration lives | files you edit | a bundle deployed from the console |
| Reload | `SIGHUP` re-reads the files | a deploy, applied within seconds |
| Admin UI | opt-in, off by default | always on, needed to provision |
| Inspect it | `check -config <dir>` | `check -data <dir>`, `config show` |

Both modes run the same validators, so a configuration that passes `check` in one
behaves the same in the other.
