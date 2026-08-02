# Central Management

A managed gateway ships with **no configuration at all**: no environment
variables, no command-line arguments, no files. It is told everything by a
console, and until it is, it does not relay mail.

That is deliberate. An edge node holds relay credentials and decides where your
mail goes; the fewer ways there are to put something on it, the fewer ways there
are to put the wrong thing on it.

## How a node joins

**1. It generates an identity.** On first boot the gateway creates an Ed25519
keypair and stores it in its data directory. The private key never leaves the
node. Its **fingerprint** — the SHA-256 of the public key — is what an operator
approves.

**2. It mints a claim code.** 100 bits, in an alphabet with no `I`, `L`, `O` or
`U` so it can be read aloud. It is logged at boot and printed by
`04-gateway.sh`, and it is what gets you into the local wizard. Until you present
it, the wizard shows exactly one page: version, fingerprint, and a field for the
code.

**3. You point it at the console.** In the wizard, give it the console URL. The
gateway registers itself. Registration is **open** — there are no tokens to copy
around — so the node lands in a `pending` state and can do nothing yet.

**4. An operator approves the fingerprint.** In the console, compare the
fingerprint the node reported against the one the node itself displays, and
approve it. This is the one step that cannot be automated away, and it is the
only thing standing between an open registration endpoint and a stranger being
handed your relay credentials.

**5. You deploy a configuration.** Assign profiles and a relay group, press
Deploy, and the node pulls the bundle within a second or two.

::: tip The claim code is not single-use
Presenting it does not consume it. A code that could be spent once, plus a
cookie, leaves the node reachable by exactly one browser for ever — and every
second operator, cleared cookie or new laptop would need a reset, which signs
everybody else out. What must not reopen is the *unauthenticated* window, and
that closes the moment a code exists.

`mailgw-go claim status` shows the current code without rotating it.
`mailgw-go claim reset` mints a new one and signs everybody out.
:::

## How configuration is modelled

**Profiles** are reusable blocks of raw configuration text, each one a file the
gateway already understands:

| Kind | Becomes | Contains |
|---|---|---|
| `server` | `server.yaml` | listeners, limits, timeouts, outbound tuning |
| `ruleset` | `routing.yaml` | your policy and routing rules |
| `allowlist` | `ngmfilter.json` | the IP allowlist |

**Relay groups** are structured rather than raw text, because their members carry
per-relay credentials and transport settings.

**Credential sets** hold inbound SMTP AUTH logins. Passwords are hashed on the
way in and only ever leave as hashes.

A gateway is assigned some of each. **Deploy** composes them into an immutable
version and points the gateway at it.

## Deploy and rollback

A deploy composes the assigned pieces into one JSON bundle, hashes it, stores it
as a new `ConfigVersions` row, and notifies the gateway. Redeploying an unchanged
configuration does not pile up versions — the digest is compared first and the
gateway is simply re-pointed.

**Rollback re-points at an older version rather than composing a new one**, so
what runs afterwards is byte-for-byte what ran before. Nothing is re-derived, and
nothing can be re-derived differently.

The gateway polls every 15 seconds and also holds a WebSocket, so a deploy
normally lands in milliseconds. The socket carries no state — it only says "go
and look" — so a missed notification costs latency and nothing else.

## What a gateway does with a bad bundle

It refuses it and keeps running the last good one.

Validation happens **on the gateway**, not in the console. The console does shape
checks only; the rule language is compiled and type-checked by the gateway
itself, which is the only thing that can be authoritative about it. A bundle that
does not compile is reported back as an `apply_error` and shown in the console,
while mail keeps flowing on the previous configuration.

Some settings can be swapped into a running process and some cannot. The
allowlist, the rules and the inbound credentials hot-swap. Listeners, TLS, the
relay table, spool settings and outbound tuning need a restart — and when a
deploy changes one of those, the console is told **which** ones, by name, rather
than being shown an unexplained "restart required".

## What travels in a bundle, and what does not

Because a managed node has no environment of its own, anything it used to read
from one now arrives in the bundle: the log service URL and API key, the
allowlist's `allow_all` flag, per-relay TLS policy and credentials, the metrics
bearer token, and inbound AUTH hashes.

**A private key never travels in a bundle.** The console keeps every version
for ever and serves it to every gateway assigned that profile, so a key placed
there would be permanently retained and fleet-wide. TLS certificate and key are
paths on the gateway's own filesystem. A managed node with no keypair configured
generates a self-signed one into its data directory.

To see what a node is actually running, with secrets redacted:

```bash
mailgw-go config show -data /var/lib/mailgw-go
```
