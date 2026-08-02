# Central Management

The console half is `webui-fastify`; the gateway half is
`mailgw-go/internal/{store,central,adminui}`. These are fixed contracts — do not
redesign one without changing the other in the same commit.

## Identity: no tokens, no shared secrets

The gateway generates an Ed25519 keypair on first boot. Registration is **open**:
anything that can reach the console may ask to join, and lands `pending`. An
operator approves a **fingerprint** — `sha256(raw 32-byte public key)`, hex.
Nothing is ever copied between the two sides by hand.

## Request signing

Every request except `POST /agent/register` carries:

```
X-GW-Id:         <gateway_uid>
X-GW-Timestamp:  <unix seconds>
X-GW-Signature:  base64(ed25519(canonical))
```

where the canonical string is, byte for byte:

```
<METHOD>\n<request-target>\n<unix-seconds>\n<sha256-hex of the raw body>
```

- `METHOD` uppercase.
- `request-target` is the path **including any query string**, exactly as sent.
- A GET has no body, so its digest is `sha256("")`.
- Skew window is **±300 s**.

`POST /agent/register` is signed the same way but with **no `X-GW-Id`** — the
console verifies against the public key in the body, proving possession without
granting anything.

Requests with a body **must** send `Content-Type: application/json`: the agent
scope installs its own raw-body parser so the signature covers the exact bytes
sent, not a re-serialisation.

## Routes

| Route | Notes |
|---|---|
| `POST /agent/register` | self-signed, idempotent per fingerprint, **can never reset an approval** |
| `GET /agent/status` | answers a pending gateway too — that is how it learns it is waiting |
| `GET /agent/config` | **403 unless approved.** The one route approval gates |
| `POST /agent/report` | applied version, `apply_error`, `restart_required`, metrics |
| `GET /agent/ws` | WebSocket in the same signed scope |

**Approval gates exactly one thing**: `GET /agent/config`. `/status` and
`/report` answer a pending gateway, because that is how it learns its state and
how the console shows it as alive.

The `/agent/*` scope sits at the **root** of the Fastify tree, outside both the
cookie gate (a gateway must not be redirected to `/login`) and the audit-log hook
(a polling fleet would flood the `Logs` table).

## The WebSocket

Exists so a deploy lands in milliseconds instead of on the 15-second poll. The
upgrade carries the same signature, so there is no new authentication surface.

**Frames carry no state** — the gateway is told "go and look" and re-asks
`/agent/status` — so a duplicated or delayed notification is harmless and a
socket that never connects costs only latency.

`deployBundle`, `rollbackTo` and `setStatus` publish to an in-process bus **after
their transaction commits**; a notification sent inside the transaction could
arrive before the row was visible. Because a second replica's deploy would not
reach that bus, each live socket also re-reads its own gateway row every 10 s.

The dialer must use an **HTTP/1.1** transport: WebSocket speaks the HTTP/1.1
upgrade, not RFC 8441 over h2 — which is why the console's `allowHTTP1: true`
matters.

## The bundle

Keys mirror the configuration directory's filenames one for one. That is the
whole design: `check`, `explain` and every existing test keep working, and a
bundle is diffable against a directory.

```jsonc
{ "format": 1,
  "server":    "<server.yaml text>"  | null,
  "routing":   "<routing.yaml text>" | null,
  "allowlist": { "allowed": [...], "allow_all": false },
  "relays":    { "Outbound": [ { "name", "exchange", "port", … } ] },
  "logging":   { "url_conn", "url_queue", "url_delivery", "api_key" },
  "admin":     { "metrics_token": "…" },
  "auth":      { "users": [ { "user", "hash" } ] } }
```

::: warning Every optional field is omitted when empty
`stableStringify` drops `undefined`, and that is the only thing keeping an
unchanged configuration hashing identically. Return `undefined`, never `{}` and
never `{users: []}` — otherwise every gateway in the fleet re-pulls and possibly
restarts for a configuration that did not change.

`src/central/bundle.test.ts` pins this. It is the invariant most likely to break.
:::

Arrays are **not** sorted by `stableStringify`, because array order is often
meaningful. Any array the console composes from a database query must be sorted
explicitly by the composer — relays by priority, credentials by username — or the
digest follows whatever order MySQL happened to return.

`bundle_sha256` is an **opaque change token**. The gateway compares it and never
recomputes it: Fastify re-serialises the bundle on the way out, so the wire bytes
are not the bytes that were hashed.

## Applying a bundle

`serveFile` and `serveManaged` converge on one `*gateway`, brought up from a
`*loaded` exactly once. The **first** apply builds the spool, event client,
runner and SMTP server and starts listening; **every later** apply swaps only the
allowlist and the compiled rules, through the same atomic pointers `SIGHUP` has
always used.

`restartRequired(live, next)` returns **a list of what changed**, not a boolean —
"restart required" with no reason is an alarm nobody can act on.

There are **three** categories, not two:

| | Examples |
|---|---|
| Hot-swapped | allowlist, compiled rules |
| Read live through an atomic pointer | metrics token, inbound AUTH credentials |
| Needs a restart | listeners, TLS, relays, spool, outbound, hostname, logging |

The middle category is deliberately absent from `restartRequired`, and a failed
apply keeps the last good value.

::: tip Boot follows recorded intent, not a timestamp
`applied_at` and `fetched_at` have one-second resolution, and a rollback lands in
the same second as the deploy it undoes — so either ordering would tie-break on
`version_id`, which is the very bundle the operator just rejected. Boot follows
`desired_version_id`, and `applied_seq` answers "most recently applied".

A **partial** apply is still marked applied, or the restart the console asked for
would boot the old bundle.
:::

## Rollback

Needs no gateway-side feature at all: the console re-points at an older version
whose bytes are still cached, so nothing is fetched and what runs afterwards is
byte-identical. Re-using cached bytes refreshes `fetched_at` so the rollback
survives a restart.

## Secrets

`auth_pass` is encrypted at rest with `CONFIG_SECRET_KEY` (AES-256-GCM, stored as
`v1:<base64>`). **The console decrypts when composing a bundle**, so no key ever
reaches a gateway.

Shipping ciphertext instead would make the key a fleet-wide shared secret held on
internet-facing relays, which is strictly worse: one compromised edge node would
yield every relay credential for every gateway.

A stored value with no `v1:` prefix is pre-migration plaintext and is read as
such, so an installation that never sets a key behaves exactly as before. A wrong
key or tampered ciphertext **throws** rather than yielding `""` — which would
send a gateway out to authenticate with an empty password.

**Inbound AUTH credentials are the opposite case.** The gateway only ever
*verifies* them, so they are bcrypt hashes and `secrets.ts` is not involved at
all. There is no key anywhere for a leaked bundle to be decrypted with.
