# Implementation plans

**One file per milestone, named `M<n>-<kebab-title>.md`.** Milestone numbers are
identity: never reused, never renumbered. A new milestone takes the next free
number even when it should be worked first — running order lives in the table
below, not in the number.

`TODO.md` files remain the running backlog (loose items, known gaps,
"deliberately not done, and why"); the milestone body lives here.

Every file carries its **status on line 3**, in the same shape:

```
**Status:** …  ·  **Package(s):** …  ·  **Depends on:** …  ·  **Blocks:** …
```

## Index

| Plan | Milestone | Package(s) | Status |
|---|---|---|---|
| [M1](./M1-gateway-core.md) | Gateway core: SMTP, spool, delivery, events | `mailgw-go` | **done** |
| [M2](./M2-routing-dsl.md) | The routing DSL | `mailgw-go/internal/ruleset` | **done** |
| [M3](./M3-central-management-server.md) | Central Management server | `webui-fastify`, `logservice/migrations` | **done** |
| [M4](./M4-local-admin-wizard.md) | Local admin UI, wizard, registration | `mailgw-go/internal/{store,central,adminui}` | **done** |
| [M5](./M5-config-pull.md) | Config pull, versioned apply + rollback, the zero-config node | `mailgw-go`, `webui-fastify`, `logservice/migrations`, `deploy` | **done** |
| [M6](./M6-observability.md) | Fleet observability | `mailgw-go/internal/obs`, `logservice`, `webui-fastify` | **done** |
| [M7](./M7-queue-completeness.md) | Queue completeness | `mailgw-go`, `logservice/migrations`, `webui-fastify`, `deploy` | **done** |
| [M8](./M8-parity-hardening.md) | Parity hardening | `mailgw-go` | **done** |
| [M9](./M9-correctness-and-durability-fixes.md) | Correctness + durability fixes (review of 2026-07-29) | `mailgw-go/internal/{smtpsrv,deliver,queue}`, `webui-fastify` | **done** (M9.5 landed with M7) |
| [M10](./M10-smtp-correctness-fixes.md) | SMTP correctness and delivery-path defects (audit of 2026-08-01) | `mailgw-go/internal/{smtpsrv,deliver,queue,config,obs,proxyproto}` | **done** |
| [M11](./M11-resource-bounds.md) | Resource bounds | `mailgw-go/internal/{smtpsrv,config,central,deliver,queue,events,obs}` | **done** |
| [M12](./M12-admin-ui-auth.md) | Admin UI authentication | `mailgw-go/internal/{adminui,store,config}`, `cmd/mailgw-go`, `webui-fastify`, `deploy` | **done** |
| [M13](./M13-inbound-auth.md) | Inbound SMTP AUTH, and inbound DSN | `mailgw-go/internal/{smtpsrv,config,ruleset,obs,dsn,queue}`, `webui-fastify`, `logservice/migrations` | **done** |
| [M14](./M14-message-authentication.md) | Message authentication: SPF, DKIM, DMARC | `mailgw-go/internal/{msgauth,smtpsrv,queue,ruleset,config,obs}`, `cmd/mailgw-go` | **done** |
| [M15](./M15-rate-limiting.md) | Rate limiting | `mailgw-go/internal/{ratelimit,smtpsrv,config,obs}`, `cmd/mailgw-go` | **done** |
| [M16](./M16-m11-reaudit-fixes.md) | Fixes from the M11 re-audit | `mailgw-go/internal/{smtpsrv,deliver,queue,events,config,obs}`, `cmd/mailgw-go` | **done** |

**Order worked:** M9 → M4 → M5 → M6 → M7 → M8 → M10. **M1–M10 are all done.**
M9.4 landed with M4 and M9.5 with M7, as the notes here suggested they should.
M8 closed the last parity gaps — attachment scanning, inbound TLS and the
audit-event replayer — so **Haraka can be retired rather than kept alongside**.

**M10 found five things its own plan had wrong**, all recorded in its "What was
built differently" section: `smtp.ErrTooLongLine` does reach the gateway during
DATA (so the branch M10 declined to write was not dead code), `internal/deliver`
has no metrics or logger to count with, a self-signed relay made an opportunistic
attempt *fail* rather than downgrade, a Logout-time connection event doubles on
every STARTTLS connection, and M10.5's "parse the PROXY header lazily" cannot
work because `Guard` reads `RemoteAddr()` inside its accept loop.

**M11 found five more**, in its own "What was built differently": the connection
cap has to sit *outside* `Guard` or it becomes the attack it was meant to stop;
bounding the bundle decode does nothing unless the deferred body drain is bounded
with it; `Deliver`'s defers run LIFO, so fixing the pooled-connection guard makes
`Pool.Put`-before-`release` load-bearing; sweeping the MX cache on write cannot
bound a workload wider than the cache, so a hard constant was needed too; and
M11.6's stale DSN diagnostic was in fact **blank** on a first attempt, not merely
out of date.

**M16 is a re-audit of M11's own code**, run before any of it was committed, and
it found that M11 had put a resource *leak* into the resource-bounds milestone:
the connection cap's wrapper hid the `*tls.Conn` from go-smtp, which dropped the
only pre-handshake read deadline in the server, so on an `implicit_tls` listener
a silent peer held its semaphore slot for ever. Twelve items in all — including
a pooled connection that a cancellation could close after another delivery had
taken it, `Pool.Put` holding the pool mutex across a QUIT, a retention sweep that
deleted evidence on the pass that filed it, and `Send` after `Close` being able
to panic the process. **The lesson worth keeping: M11's tests all construct their
subject directly, and three of its seven items only take effect through
`cmd/mailgw-go`'s wiring, which had no test at all.** M16 adds one.

**Order worked from here:** **M11 → M16 → M12 → M13 → M14 → M15**. With M15 in,
**every milestone in this directory is done.**
M11 was taken first because it is self-contained and touches only the gateway,
and M16 followed immediately because it is that same code. M12 is the security
item `mailgw-go/TODO.md` ranked first and it unblocks two things held behind it.
M13 precedes M14 because authenticated submission changes what the
message-authentication policy should say. Once again the numbers are identity:
M11 and M16 being worked before M12 does not renumber anything, exactly as M9 was
worked first without moving.

**M15 finished the pair M11 started.** `max.connections` bounds how many
connections exist at once; rate limits bound how often anything happens at all —
per IP, per sender, per authenticated user, per recipient domain and per failed
AUTH — because a peer opening one connection at a time and pushing a million
messages through it never trips a concurrency cap. Every limit ships off, every
refusal is a 4xx, and they are read live so an operator can retune one during an
incident without restarting a mail server. **The most interesting thing it found
is that M11's own placement rule inverts here**: the connection cap has to sit
*outside* the allowlist because it bounds a shared semaphore, and the rate
limiter has to sit *inside* it because it bounds a map — outside, that map is
keyed by the internet and the limiter becomes the memory-exhaustion vector it
was added to prevent. It also chose token buckets over the sliding windows its
plan named, which is what makes eviction provable: a bucket refilled to capacity
is byte-for-byte a fresh one, so dropping it cannot release anybody.

**M14 gave the gateway an opinion about who a message is from.** SPF at MAIL,
DKIM and DMARC at DATA, an `Authentication-Results` header recording all three,
and DKIM signing on the way out from keys that live on the gateway's own disk —
each result a rule field (`spf.*`, `dkim.*`, `dmarc.*`) rather than a config
boolean, so "reject on DMARC fail" is something an operator writes and nothing
is refused that was not refused before. **Its own plan was wrong in four
places**, all in its "What was built differently": the results headers cannot go
in `receivedHeader()`, because that is written before DATA is read and the DKIM
result does not exist yet; signing cannot go in `internal/deliver`, because
`Message.Body` is a one-shot reader and a signature has to precede bytes the
signer already consumed; stripping *every* inbound `Authentication-Results`
overshoots RFC 7601 §5, which asks only for the ones forging our own
authserv-id; and SPF was worth a library rather than the hand-roll it suggested.
Two things it did not anticipate turned out to be load-bearing: a check that did
not run reads as **absent** rather than `none`, and only signatures that
*verified* contribute a `d=` — without that second one a forger passes DMARC by
attaching a broken signature naming the victim.

**M13 closed the last inbound gap and found two defects in shipped code doing
it.** A gateway now advertises AUTH PLAIN and AUTH LOGIN against bcrypt hashes
the console issues and the bundle carries, so submission-with-credentials exists
where the IP allowlist used to be the only inbound gate — and
`auth.user`/`auth.mechanism` left the `unpopulated` registry, which is what
"done" means for this one. Its own plan had them at the wrong *stage*: helo
policy runs inside `Backend.NewSession`, before a client can possibly have
authenticated, so a rule reading them could never have matched. It also honours
inbound RFC 3461 DSN end to end — ORCPT through to `Original-Recipient`, NOTIFY
deciding who is told, RET choosing how much comes back, and `NOTIFY=SUCCESS`
earning a "relayed" report, because this gateway does not pass DSN parameters on
and §5.2.7 makes it the boundary that answers for them. The two defects were
both invisible until something could exercise them: `Original-Envelope-Id` was
being answered with this gateway's own uuid rather than the sender's ENVID, and
two notifications about one envelope **overwrote each other's body on disk**,
which a delay warning and a later failure could already do.

**M12 found that its own plan contained a dead end.** It specified that
presenting the claim code "consumes" it; with a cookie as the only other
credential, that leaves a node reachable by exactly one browser for ever, and
every second operator, cleared cookie or new laptop needs `claim reset` — which
signs everybody else out. The unauthenticated window closes when a code
*exists*, not when one is *spent*, so the code stays valid and `admin_claimed_at`
only records first use, which is what stops a live credential being re-logged on
every boot. Six other departures are in its own "What was built differently",
including two that are traps rather than preferences: an attempt counter in the
store would have made `POST /claim` an unauthenticated write to the gateway's
own database, and a `Secure` cookie on a plain-HTTP listener is simply never
sent, so the UI would have signed nobody in.

**M5 grew past its own plan.** The shipped gateway is now zero-configuration —
no environment, no arguments, no files — which pulled the logservice API key,
relay TLS policy and `allow_all` out of the gateway and into what Central
Management serves, and added a WebSocket notification channel. M5's own "What
was built differently" section is the authority; the contract notes below are
still accurate except where it says otherwise.

M1's plan is **reconstructed** — that milestone predates this directory and was
never written up. It records the finished state, not the plan that produced it.

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
  "allowlist": { "allowed": ["10.0.0.0/8", "::1"], "allow_all": false },
  "relays":    { "Outbound": [ { "name":"mx1", "exchange":"host", "port":25,
                                 "priority":0, "auth_user":"u", "auth_pass":"p",
                                 "auth_pass_env":"...", "tls":"required",
                                 "allow_insecure_auth":false } ] },
  "logging":   { "url_conn":"...", "url_queue":"...", "url_delivery":"...",
                 "api_key":"..." },
  "admin":     { "metrics_token":"..." },
  "auth":      { "users": [ { "user":"app@ngm.dev", "hash":"$2b$10$..." } ] } }
```

Every optional field is **omitted when empty**, so an unchanged configuration
keeps hashing identically. The fields beyond the original five were added in M5
because a zero-configuration node has no environment to read them from —
`allow_all` in particular, without which an allow-all gateway could not be
expressed from the console at all.

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
- **Minimal dependencies.** The module had four direct deps when M4–M6 were
  written (`google/uuid`, `emersion/go-smtp`, `emersion/go-sasl`,
  `sigs.k8s.io/yaml`) and has **six** today — M4 added `modernc.org/sqlite` and
  M5 `coder/websocket`, both argued for in their own files.
  `net/http.ServeMux` has had method+wildcard patterns since Go 1.22 and
  `html/template` + `embed` are stdlib, so the admin UI adds nothing. **M13
  made it seven**: `go-sasl` was already there for the outbound side as the plan
  said, but verifying a password is not SASL's job — `golang.org/x/crypto/bcrypt`
  is what checks the hash the console issues, and one hashing story across the
  product beat inventing a stdlib PBKDF2 encoding for both sides to agree on.
  **M14 made it nine**, and argued both in its own file: `emersion/go-msgauth`
  (DKIM, DMARC, `Authentication-Results` — same author as `go-smtp` and
  `go-sasl`, and its three packages import nothing but stdlib and
  `golang.org/x/crypto/ed25519`, so it added no indirect build) and
  `blitiri.com.ar/go/spf`, which the plan had expected to hand-roll. Note the
  second one carries `gopkg.in/yaml.v3` for its conformance test only: `go list
  -deps ./...` must keep showing that absent from the build graph on an upgrade.
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
