# Central Management — implementation plans

Working plans for the milestones that move mailgw-go off local config files and
onto central management. **M3 is done** (the console half, in `webui-fastify`);
these are the three that follow.

| Plan | Milestone | Package(s) | Status |
|---|---|---|---|
| — | **M3** — Central Management server | `webui-fastify`, `logservice/migrations` | **done** |
| [M4](./M4-local-admin-wizard.md) | Local admin UI, wizard, registration | `mailgw-go/internal/{store,central,adminui}` | not started |
| [M5](./M5-config-pull.md) | Config pull, SQLite cache, versioned apply + rollback | `mailgw-go/internal/{config,ruleset}`, `cmd/mailgw-go` | not started |
| [M6](./M6-observability.md) | Fleet observability | `mailgw-go/internal/obs`, `logservice`, `webui-fastify` | not started |

M7 (queue completeness) and M8 (parity hardening) are the pre-existing backlog
and stay tracked in `mailgw-go/TODO.md`; they are unaffected by this work and
have no plan file here yet.

---

## What M3 already established

These are fixed contracts. M4 and M5 implement the other side of them; do not
redesign them without changing `webui-fastify` in the same commit.

**Identity — no tokens, no shared secrets.** The gateway generates an Ed25519
keypair on first boot. Registration is *open*: anything that can reach the
console may ask to join, and lands as `status = 'pending'`. An operator approves
a **fingerprint** = `sha256(raw 32-byte public key)`, hex. Nothing is ever
copied between the two sides by hand.

**Request signing.** Every request except `/agent/register` carries:

```
X-GW-Id:         <gateway_uid>          (uuid, returned by /agent/register)
X-GW-Timestamp:  <unix seconds>
X-GW-Signature:  base64(ed25519(canonical))
```

where the canonical string is, byte for byte:

```
<METHOD>\n<request-target>\n<unix-seconds>\n<sha256-hex of the raw body>
```

- `METHOD` is uppercase.
- `request-target` is the path **including any query string**, exactly as sent
  (`/agent/status`, not `https://host/agent/status`).
- A GET has no body, so its digest is `sha256("")` =
  `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
- Skew window is **±300s** (`SIGNATURE_SKEW_SECONDS` in
  `webui-fastify/src/agent/verify.ts`).

`POST /agent/register` is signed the same way but with **no `X-GW-Id`** — the
console verifies against the public key in the body, which proves possession of
the private key without granting anything.

Requests with a body **must** send `Content-Type: application/json`: the agent
scope installs its own raw-body parser, and the signature covers the exact bytes
sent, not a reserialisation.

**Wire formats** (`webui-fastify/src/routes/agent.ts`,
`src/validation/agent.ts`):

```jsonc
// POST /agent/register  -> 201 (new) or 200 (already known)
{ "pubkey": "<base64 of 32 raw bytes>", "hostname": "...", "os": "linux",
  "arch": "amd64", "cpus": 4, "mem_bytes": 8000000000,
  "ip_addrs": ["10.0.0.5"], "version": "0.2.0" }
-> { "status":"ok", "gateway_uid":"...", "fingerprint":"...", "approval":"pending" }

// GET /agent/status -> 200 (answers a pending gateway too)
{ "status":"ok", "approval":"pending|approved|rejected|revoked",
  "desired_version_id": 42, "desired_version": 7,
  "bundle_sha256": "...", "applied_version_id": 41 }

// GET /agent/config -> 200 | 403 (not approved) | 404 (nothing deployed)
{ "status":"ok", "version_id":42, "version":7, "bundle_sha256":"...",
  "bundle": { /* see below */ } }

// POST /agent/report -> 200
{ "applied_version_id": 42|null, "apply_error": "..."|null,
  "restart_required": false, "version": "0.2.0", "metrics": { "...": 0 } }
```

**The bundle** (`webui-fastify/src/central/bundle.ts`). Its keys mirror the
config-directory files one-for-one, which is the whole point — `check`,
`explain` and every existing test keep working:

```jsonc
{ "format": 1,
  "server":    "<server.yaml text>"   | null,
  "routing":   "<routing.yaml text>"  | null,
  "allowlist": { "allowed": ["10.0.0.0/8", "::1"] },
  "relays":    { "Outbound": [ { "name":"mx1", "exchange":"host", "port":25,
                                 "priority":0, "auth_user":"u", "auth_pass":"p" } ] },
  "logging":   { "url_conn":"...", "url_queue":"...", "url_delivery":"..." } }
```

`bundle_sha256` is over a **depth-sorted stable stringify** of that object, so
an unchanged configuration hashes identically and the status poll stays cheap.
The gateway should treat the digest as opaque and compare it, not recompute it.

**Approval gates exactly one thing:** `GET /agent/config`. `/status` and
`/report` answer a pending gateway, because that is how it learns it is waiting
and how the console shows it as alive.

---

## Constraints that shape all three plans

- **Pure Go only.** The image is `gcr.io/distroless/static-debian12:nonroot`
  built with `CGO_ENABLED=0`. Any cgo SQLite driver (`mattn/go-sqlite3`) is
  out — see M4 for the choice made.
- **Minimal dependencies.** The module has four direct deps today
  (`google/uuid`, `emersion/go-smtp`, `emersion/go-sasl`, `sigs.k8s.io/yaml`).
  `net/http.ServeMux` has had method+wildcard patterns since Go 1.22 and
  `html/template` + `embed` are stdlib, so the admin UI adds nothing.
- **File mode must not regress.** `-config <dir>` keeps working byte-identically.
  It is what `check`, `explain`, `testdata/config`, `internal/smtpsrv/contract_test.go`
  and the Bun SMTP e2e suite all run on. Every plan below states what it must not
  break; verify with `pnpm test:mailgw-go` and `SMTP_PORT=2525 bun test tests/smtp`.
- **Reload semantics are already decided** (`cmd/mailgw-go/main.go` `watchReload`):
  all-or-nothing, keep the running configuration on any failure, and only the
  allowlist and the compiled ruleset are hot-swappable via `atomic.Pointer`.
  Anything else — relay table, listeners, TLS, spool dir, outbound tuning —
  needs a restart. M5 preserves this rather than working around it.
- **The mail path never waits on management.** Registration, polling and
  heartbeats are as non-blocking and failure-tolerant as `internal/events` is:
  if the console is down, mail keeps flowing on the last-good configuration.

## Suggested order

M4 → M5 → M6, and they are genuinely sequential: M5's pull loop needs M4's
store and signing client, and M6's heartbeat needs M5's applied-version state to
report. Within M4 the store and the signing client can be built and tested
before any UI exists — `POST /agent/register` against a running console is a
better first integration test than a rendered page.
