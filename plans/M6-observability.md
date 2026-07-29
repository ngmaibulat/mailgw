# M6 — Fleet observability

**Packages:** `mailgw-go`, `logservice`, `webui-fastify`  ·  **Depends on:** M5

Read [README.md](./README.md) for the `/agent/report` contract.

## Goal

Tell a healthy gateway from a wedged one, from the console, without SSH. Three
strands: **counters** in the gateway, **push** to Central Management, and a
**`gateway` column** in the log tables so per-gateway log views are possible at
all.

This milestone also absorbs the `/healthz` + Prometheus item that used to sit
last in the old M6 — with a fleet rather than one box, it is no longer optional.

## The starting point

There is almost nothing to build on. The **only** counters in the whole binary
are `events.Stats` (`internal/events/client.go:19`) — `Queued, Sent, Dropped,
Spilled, Rejected, Retried, SpillFail` — whose doc comment already says "for
logging and /metrics". Everything about the mail path itself is uncounted.

`internal/obs/` exists and is **empty**. That is its purpose.

## Work

### 6.1 `internal/obs` — the counter registry

```go
package obs

// Metrics is the process-wide counter set. All fields are atomic and safe to
// increment from any goroutine on the SMTP or delivery path.
type Metrics struct {
    // connections
    ConnAccepted atomic.Int64
    ConnDenied   atomic.Int64   // allowlist rejection, before the banner

    // sessions / messages
    MsgAccepted    atomic.Int64
    MsgRejected    atomic.Int64  // a policy rule said no
    MsgTempfailed  atomic.Int64
    MsgDiscarded   atomic.Int64
    MsgQuarantined atomic.Int64
    RcptAccepted   atomic.Int64
    RcptRejected   atomic.Int64
    BytesIn        atomic.Int64

    // queue / delivery
    EnvelopesQueued  atomic.Int64
    DeliverAttempts  atomic.Int64
    DeliverOK        atomic.Int64
    DeliverDeferred  atomic.Int64
    DeliverBounced   atomic.Int64
    DeliverConnFail  atomic.Int64  // Err != nil: dial/TLS/AUTH/MAIL FROM
}

func New() *Metrics
func (m *Metrics) Snapshot() map[string]int64   // for the heartbeat payload
func (m *Metrics) WritePrometheus(w io.Writer, gauges Gauges)
```

Deliberately **not** a metrics library. Prometheus text format for counters is
three lines each; pulling in `prometheus/client_golang` for that would be the
single largest dependency in the module and this project's style is emphatically
against it. Revisit only if histograms are wanted.

Gauges are read at scrape time rather than counted:

```go
type Gauges struct {
    QueueReady    int   // from Spool.Len()
    QueueInflight int
    ConfigVersion int   // applied version, for "is this box up to date"
    Approved      bool
}
```

`Spool.Len() (ready, inflight int, err error)` already exists
(`internal/queue/spool.go:288`). Note it counts **only** `q/` and `inflight/` —
`quarantine/` and `dead/` are excluded, which is a gap worth closing here since
"envelopes stuck in quarantine" is exactly the kind of thing a console should
surface. Add `Spool.LenAll()` or extend the existing signature.

### 6.2 Instrumentation points

Thread a `*obs.Metrics` through the existing structs (`smtpsrv.Backend`,
`queue.RunnerConfig`) the same way `*slog.Logger` already is. Nil-safe or
always-constructed — pick one and be consistent; always-constructed is simpler.

| Counter | Where |
|---|---|
| `ConnAccepted` / `ConnDenied` | `internal/smtpsrv/listener.go` — `Accept()` accepts, `deny()` denies. This is the only place that sees a connection before the banner. |
| `MsgAccepted` | `session.Data` success path, at `queuedReply()` (`session.go:479`) |
| `MsgRejected` / `MsgTempfailed` / `MsgDiscarded` / `MsgQuarantined` | `session.applyTerminal` (`session.go:188`), switching on the action — one place covers every terminal action at every stage |
| `RcptAccepted` / `RcptRejected` | `session.Rcpt` (`session.go:263`) and `session.rcptTerminal` (`session.go:315`) |
| `BytesIn` | `session.Data`, from the size `WriteBody` returns |
| `EnvelopesQueued` | `session.split` result / `Spool.Enqueue` call site |
| `DeliverAttempts` | `Runner.attempt` entry (`runner.go:224`) |
| `DeliverOK` / `DeliverDeferred` / `DeliverBounced` | `Runner.record` (`runner.go:336`) — it already classifies per-recipient `deliver.Outcome` |
| `DeliverConnFail` | `Runner.attempt` where `Result.Err != nil` (the "try the next relay in the group" branch) |

`Runner.record` is the right single place for the delivery three because it
already walks `Result.Rcpts` and knows each `Outcome`; counting anywhere else
risks double-counting across failover attempts.

### 6.3 `/metrics` and `/healthz` on the admin server

Both on the M4 admin server (`internal/adminui`), which is already listening.

- `GET /healthz` — already added in M4. Keep it **liveness-only**: no DB, no
  console call. A console outage must not get the process killed by an
  orchestrator. (Same reasoning as `webui-fastify`'s `/health`.)
- `GET /readyz` — new, and the useful one for a fleet: ready = provisioned,
  approved, a config applied, and SMTP listening. Returns the reasons when not.
- `GET /metrics` — Prometheus text, `text/plain; version=0.0.4`. Include
  `mailgw_build_info{version="…",commit="…"} 1` and
  `mailgw_config_version{} <n>` so a scrape answers "which build, which config".

Prefix everything `mailgw_`. Counters get `_total`. Keep names stable — renaming
a metric breaks every dashboard downstream, so it is worth spending five minutes
on the names now.

### 6.4 Heartbeat push

Model it on `internal/events.Client`, which is exactly the right shape and
already battle-tested in this codebase: bounded, non-blocking, never fails the
mail path.

- Ticker (default 30s, jittered) → `central.Report{version, applied_version_id,
  apply_error, restart_required, metrics: m.Snapshot()}`.
- **Fold it into the M5 pull loop rather than running a second goroutine.** The
  poll already contacts the console every 30s and already reports applied state;
  adding the metrics map to that payload costs nothing and halves the request
  rate. Separate tickers only if the poll interval ever needs to differ.
- Failure is a WARN and a dropped sample. Never retry a heartbeat — a stale
  metrics snapshot is worse than a missing one, and the next tick is 30s away.

Console side: `POST /agent/report` **already accepts and validates a `metrics`
object** (`webui-fastify/src/validation/agent.ts`) and currently discards it.
Persisting it is the remaining work:

- Simplest useful thing: a `GatewayMetrics` table, one row per gateway holding
  the latest snapshot as JSON plus `updated_at`. Answers "what is this gateway
  doing right now", which is 90% of the value.
- A time series is a bigger decision (retention, rollup) and should not be
  smuggled in — if it is wanted, it belongs in logservice with its own
  migration and purge job, alongside `purgeOldLogs`.

### 6.5 The `gateway` column

`logservice/migrations/022_add_gateway_column.sql`:

```sql
ALTER TABLE `Connection`  ADD COLUMN `gateway` VARCHAR(64);
ALTER TABLE `Transaction` ADD COLUMN `gateway` VARCHAR(64);
ALTER TABLE `Delivery`    ADD COLUMN `gateway` VARCHAR(64);
CREATE INDEX `idx_connection_gateway`  ON `Connection`  (`gateway`);
CREATE INDEX `idx_transaction_gateway` ON `Transaction` (`gateway`);
CREATE INDEX `idx_delivery_gateway`    ON `Delivery`    (`gateway`);
```

This closes the "No gateway column in the log tables" gap in
`mailgw-go/TODO.md` — today Haraka and mailgw-go rows are distinguishable only
by the `X-NGM-Gateway` header on relayed mail, which is invisible from the log
viewer. With a fleet rather than one gateway it stops being a nicety.

Coordinated changes, all required together:

| Where | Change |
|---|---|
| `mailgw-go/internal/events/payload.go` | add `Gateway string \`json:"gateway,omitempty"\`` to `Connection`, `Queue`, `Delivery` |
| `mailgw-go` session/runner | populate it from the store's `gateway_uid` (fall back to hostname in file mode, so file-mode rows are still labelled) |
| `logservice/src/models/{connection,transaction,delivery}.ts` | include the column in the INSERTs |
| `logservice/src/validation/delivery.ts` | accept the optional field |
| `logservice/src/query/search.ts` | add `gateway` to `CONNECTION_FIELDS`, `TRANSACTION_FIELDS`, `DELIVERY_FIELDS` — **without this the column is not filterable**, since `buildWhere` silently skips fields not in the allowlist |
| `webui-fastify/public/js/grids/{connection,delivery,mails}.js` | add a `LogGrid.text("gateway", "Gateway")` column |

`webui-fastify`'s `/api/*` proxy needs no change: it forwards the search payload
verbatim and logservice owns the field allowlist.

Backfill: existing rows get `NULL`. Do not attempt to guess — a `NULL` gateway
honestly means "written before this column existed".

### 6.6 Fleet dashboard

Extend the console's `/` dashboard (`templates/pug/index.pug`) with a fleet
card: counts by status (pending / approved / stale), any gateway whose
`last_seen` is older than ~3 poll intervals, any with a non-null `apply_error`,
and any with `restart_required`. "Pending approvals" should be visually loud —
it is the one thing that needs a human.

`/gateways` gains the latest metrics snapshot per row (queue depth, delivered
since boot) and a **stale** pill computed from `last_seen`.

## Tests

- `internal/obs`: `Snapshot` keys are stable (a golden list — renaming a key
  silently breaks the console); `WritePrometheus` output parses as valid
  exposition format (assert against a small hand-written expected block).
- `internal/smtpsrv`: extend `contract_test.go` — an allowlist denial increments
  `ConnDenied` and not `ConnAccepted`; a policy reject increments `MsgRejected`;
  a successful message increments `MsgAccepted` and `EnvelopesQueued`.
- `internal/queue`: a deferred delivery increments `DeliverDeferred` exactly
  once even across relay failover within one attempt (the double-count guard).
- `logservice`: a POST carrying `gateway` stores it; a search filtering on
  `gateway` returns only matching rows (the field-allowlist regression guard —
  this is the failure mode where the filter silently matches everything).
- `webui-fastify`: `app.inject()` for the fleet dashboard rendering with a
  stale/errored gateway present.

## Verification

```bash
cd mailgw-go && go build ./... && go vet ./... && go test -race ./...
cd ../logservice && bun test tests/
docker compose run --rm db-migrator          # applies 022
```

End to end:

1. Send mail through an approved, deployed gateway (`bun test tests/smtp`).
2. `curl -s localhost:8080/metrics | grep mailgw_` — connection, message and
   delivery counters have all moved; `mailgw_queue_ready` is 0 once drained.
3. `curl -s localhost:8080/readyz` returns ready; stop the console and confirm
   it **stays** ready (a console outage is not a gateway outage) while
   `/gateways` shows it going stale.
4. In the log viewer, the new **Gateway** column is populated, and filtering on
   it narrows the result set (compare `total` with and without the filter — a
   silently-ignored filter returns the same total, which is the bug this
   guards against).
5. Break a deploy (bad ruleset). The fleet card shows the gateway in error, and
   the detail page shows the compiler message.
6. Kill a gateway. Within ~3 poll intervals it shows **stale** on the dashboard.

## Notes and follow-ups

- **`Spool.Len()` excludes `quarantine/` and `dead/`.** Fix while adding the
  gauge, or the console will under-report stuck mail. Related: `quarantine/`
  still has no release path (M7) — a console button for it is the obvious pairing
  once the gauge makes the backlog visible.
- **Route decisions are still not in the audit events** (`mailgw-go/TODO.md`),
  so the log tables cannot answer "which rule sent this message here?". It needs
  another logservice column and pairs naturally with this migration — consider
  doing both in `022` rather than a second ALTER later.
- `/metrics` is unauthenticated on the admin listener, which binds `0.0.0.0`
  (M4) with the firewall as the access control. The exposition includes traffic
  volumes, queue depth and — via `mailgw_build_info` — the running version, so
  treat port 8080 as management-network-only, the same as the wizard. Whatever
  auth M4's follow-up adds to the admin UI should cover `/metrics`, with the
  caveat that a Prometheus scraper needs a credential it can present
  non-interactively (a bearer token, not a session cookie).
