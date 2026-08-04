# Installation

There are three ways to run mailgw, in increasing order of how much you have to
decide up front.

## Try it locally

A gateway cannot be configured from the machine it runs on, so "try it locally"
means bringing up the console too. From a checkout:

```bash
pnpm certs                   # optional: give the console a TLS pair (it self-signs otherwise)
docker compose up -d         # mariadb, log service, gateway, console, MailHog
pnpm provision               # create profiles, approve the node, deploy
```

`docker-compose.yaml` at the repository root brings up everything, and
`pnpm provision` walks the gateway through the same provisioning an operator
does by hand: it creates the first admin, a relay group pointing at MailHog and
the three config profiles, approves the gateway's fingerprint, and deploys. It
is idempotent.

Send it something:

```bash
swaks --server localhost:2525 --from you@example.com --to someone@ngm.dev
```

The gateway takes host port 25 and also publishes 2525. The console is on HTTPS
with a self-signed certificate.

::: tip Why there is no single-binary quickstart
Running `mailgw-go` on its own gives you a node that boots, generates an
identity, and waits — it will not relay until a console has deployed a
configuration to it. That is the same behaviour a production edge node has, and
it is deliberate: a gateway with no allowlist would deny every peer anyway, and
a listener that can only reject looks healthy to a load balancer.
:::

Once it is running, `mailgw-go check` prints what the gateway understood — which
listeners, which relay groups, which rules, and any warnings — reading the
configuration it actually has cached.

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

Back up **both** host directories, for different reasons:

- **`/opt/mailgw-go/data`** holds the node's private key, its claim code and its
  configuration cache. Lose it and the node is a stranger to the console: it
  must be registered and approved again.
- **`/opt/mailgw-go/queue`** is the outbound spool — mail already answered `250`
  but not yet delivered, plus anything quarantined. Lose it and you lose mail a
  sender has been told is queued.

## How a gateway is configured

There is one way, and it is worth stating plainly because it constrains
everything else on this page.

| | |
|---|---|
| How it starts | with no arguments at all |
| Where configuration lives | a bundle deployed from the console, cached in SQLite under `/var/lib/mailgw-go` |
| How it changes | a deploy, applied within seconds |
| Reload | `SIGHUP` re-applies from the cache |
| Admin UI | always on; it is how a node is provisioned |
| Inspect it | `check`, `config show`, `explain`, `mailq` — all read the cache |

The gateway reads **no environment variables, no command-line configuration and
no configuration files**. Nothing on the host it runs on can change what it
does; only the console can.

::: tip This used to be two modes
Earlier versions also accepted `serve -config <dir>` and read a directory of
files. That is gone. If you are following an older runbook, the files it tells
you to edit are now **config profiles** in the console — same names, same
contents, see [Central Management](/guide/central-management).
:::
