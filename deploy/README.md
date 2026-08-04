# deploy

Production deployment for the **modern stack**: `mailgw-go` (SMTP gateway),
`logservice` (event API) and `webui-fastify` (admin UI + Central Management).

The Haraka-era deployment lives in [`../legacy/deploy/`](../legacy/deploy/) and is
frozen — it deploys `ngmaibulat/mailgw` (Haraka) and has no webui.

## Topology

Two roles, deployed separately:

| Dir | Role | Runs | Ports |
|---|---|---|---|
| `core/` | one per install | mariadb + logservice + webui | 3000 (logservice), 4000 (webui, TLS) |
| `gateway/` | one per edge node | mailgw-go | 25 (SMTP), 8080 (admin UI), host networking |

Edge nodes POST their connection/queue/delivery events to the core node's
logservice, and — once approved in the webui — pull their entire configuration
from the core node's `/agent/*` API. **An edge node holds no configuration of
its own**: no environment, no arguments, no files. What it runs on is a signed,
versioned bundle it caches locally, so a core-node outage does not stop mail.

`common/install-docker.sh` is shared by both and installs Docker Engine + the
compose plugin on Ubuntu.

## Core node

```bash
cd deploy/core
cp .env.example .env && $EDITOR .env      # set CORE_HOST, API_KEY, SIGN_COOKIE, DB_*
mkdir -p certs                            # then drop server.key + server.crt in
bash deploy.sh
```

`CORE_HOST` must be this host's address **as the edge nodes see it** — it is
baked into the config bundles Central Management hands to gateways, so
`localhost` is wrong unless everything is on one box. `API_KEY` travels in those
bundles too, because an edge node has no environment to read it from.

`CONFIG_SECRET_KEY` (optional, `openssl rand -base64 32`) encrypts relay
passwords at rest in the database. The console decrypts them when it composes a
bundle, so the key never leaves this host and no gateway ever holds it. Leaving
it empty stores passwords in the clear, exactly as before — nothing breaks, but
a database backup then carries somebody else's credentials. Rotating it makes
existing passwords unreadable: re-enter them in the UI, or keep the old key.

The webui serves HTTP/2 over TLS. Drop a real pair in `deploy/core/certs/` as
`server.key` and `server.crt` — it is used exactly as given, and a certificate
renewed in place is picked up on the next `docker compose restart webui`. If the
directory is empty the console **self-signs a placeholder** so a new node comes
up rather than crash-looping; that certificate authenticates nothing and is
worth replacing before anybody signs in. A self-signed pair from the repo, if
that is what you want:

```bash
pnpm certs && cp certs/generated/webui/server.{key,crt} deploy/core/certs/
```

Then open `https://<core-host>:4000/setup` once to create the first admin.

Upgrades (pulls new images and re-runs migrations): `bash upgrade.sh`.

## Edge node

An edge node is **zero-configuration**. There is nothing to write, nothing to
template and nothing to keep in sync: the container starts with no command, no
environment and no config files, generates its own identity, and takes
everything it runs on from Central Management.

```bash
cd deploy/gateway
bash 00-packages.sh                       # swaks/tcpdump/vim for debugging
bash 01-docker.sh                         # Docker + compose
bash 02-checkdirs.sh                      # /opt/mailgw-go/{queue,data}
bash 03-cleanup.sh                        # remove any previous container
bash 04-gateway.sh                        # pull and start
```

`04-gateway.sh` takes no arguments and prints what to do next. The node is
running but **deliberately not relaying** until it is provisioned:

1. Open `http://<edge-node>:8080` and paste the **claim code** the script
   printed. It is also in the gateway's own log (`docker logs mailgw-go`) and
   available from `mailgw-go claim status`, and it is re-printed on every start
   until somebody uses it.
2. Enter the Central Management URL (tick the trust checkbox if the console
   serves a self-signed certificate) and press **Register**. The page then shows
   the gateway's fingerprint — which is also visible before signing in, so a
   gateway can be pre-approved.
3. In the console, open `/gateways`, find the matching fingerprint, approve it.
4. Assign a rule set, an allowlist and a relay group, and press **Deploy**. The
   gateway applies it within a second and starts listening on port 25.

A gateway with no allowlist would deny every peer anyway, and a listener that
can only reject looks healthy to a load balancer — so it fails closed and says
so on its status page.

### Looking at what a node is running

```bash
docker logs -f mailgw-go
docker exec mailgw-go /usr/local/bin/mailgw-go config show   # the bundle, secrets redacted
docker exec mailgw-go /usr/local/bin/mailgw-go check         # is it valid?
docker exec mailgw-go /usr/local/bin/mailgw-go explain --rcpt user@example.com
docker exec mailgw-go /usr/local/bin/mailgw-go claim status  # the admin UI's code
docker exec mailgw-go /usr/local/bin/mailgw-go claim reset   # lost it: mint a new one
```

`claim status` prints a live credential, so treat its output like a password
rather than a log line to paste into a ticket. `claim reset` signs every session
out as well as rotating the code — that is what it is for.

The container runs with `network_mode: host` and `user: "0:0"` on purpose — see
the comments in `gateway/docker-compose.yaml` for why neither is incidental.

### Deploys, rollbacks and failures

- A **deploy** reaches the gateway in milliseconds over a signed WebSocket, with
  a 15-second poll behind it as a fallback. A rule or allowlist change applies
  without a restart and without interrupting mail.
- A **rollback** in the console repoints at an older version whose bytes the
  gateway already holds, so what runs afterwards is byte-identical to what ran
  before, and it survives a restart.
- A **bad configuration** never takes the node down. The gateway is
  authoritative for validation: it compiles the rules on pull, keeps its
  last-good configuration if they do not, and the compiler's message appears in
  the console as `apply_error`.
- Changes the process cannot pick up live — the relay table, `listen`, TLS, the
  spool — are applied as far as they can be and reported as `restart_required`
  with a list of what changed. `docker restart mailgw-go` picks them up.
- A **console outage is a non-event.** The node boots from its cache and keeps
  relaying. When the console returns it reconciles with no operator action.

### The admin UI is always listening, and the firewall is still required

This is the one thing to get right before an edge node goes into service.

Because the wizard is the only way to provision a zero-configuration node, the
UI cannot be off by default. It is locked by a **claim code** the gateway
generates on first boot and writes to its log; presenting it in the wizard signs
an operator in, and every state-changing request afterwards needs that session
and a CSRF token. The code is not consumed — a code good for one browser only
would leave the node unmanageable from anywhere else — and `mailgw-go claim
reset` rotates it and signs everybody out.

**Run the firewall anyway:**

```bash
MGMT_CIDR=10.0.0.0/24 bash 05-firewall.sh
```

What an authenticated caller can do is re-point this gateway at a Central
Manager of their choosing and be handed that manager's configuration — and this
node's relay credentials on the way out. The usual mitigations are absent here:
`network_mode: host` means there is no docker port mapping to narrow, and the
process runs as uid 0 on a host that accepts mail from the internet. Narrowing
who can reach an authenticated root process is worth doing on its own.

Run it from a host the rule will still admit — locking yourself out is the
failure mode.

The listener is plain HTTP, deliberately. Serving it with a self-signed
certificate would authenticate nothing and would teach an operator to click
through a browser warning on the exact page where they type a secret; a real
certificate for the admin listener is tracked as a follow-up. The claim code
therefore crosses the management network in the clear, once.

**Upgrading a node from before this existed:** it starts unclaimed, mints a code
and prints it. Read it once out of `docker logs mailgw-go` — or re-run
`04-gateway.sh`, which prints it — and the UI works as before.

### Back up both `/opt/mailgw-go` directories

**`/opt/mailgw-go/data`** holds the gateway's Ed25519 private key, its admin
claim code **and** its cached configuration — the things that must survive a
container replacement. Lose it and the node re-registers as a stranger and stops
relaying until an operator approves it again. `02-checkdirs.sh` creates it
`0700` and refuses to continue if the mode is wider.

**`/opt/mailgw-go/queue`** is the outbound spool: mail this gateway has accepted
and answered `250` for but not yet delivered, plus anything quarantined or
buried. Losing it loses mail a sender has already been told is queued. It is
mounted at `/var/lib/mailgw-go/queue` inside the container — beside the store,
not at `/opt` — because that is where a gateway spools when the deployed server
profile does not name a directory, which is the normal case.
