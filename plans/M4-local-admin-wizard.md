# M4 — Local admin UI, wizard, registration

**Package:** `mailgw-go`  ·  **Depends on:** M3 (done)  ·  **Blocks:** M5, M6

Read [README.md](./README.md) first — the signing contract and wire formats it
describes are fixed by M3 and implemented here.

## Goal

A mailgw-go instance boots with **no configuration at all**, serves a local
admin UI, and the operator uses that UI to point it at a Central Manager. The
gateway generates its own identity, registers, and waits for approval. After
this milestone the gateway can register and report its state; actually *pulling*
configuration is M5.

Success looks like: `docker run` a fresh gateway with an empty data volume, open
`http://localhost:8080`, type a central URL, press Register, and see the gateway
appear as **pending** in the console at `/gateways` with a matching fingerprint.

## Decisions taken

**No env vars for bootstrap.** The wizard is the bootstrap. Two new *flags*
follow the existing `-config` precedent (`mustParse`, `cmd/mailgw-go/main.go:73`):

| Flag | Default | Purpose |
|---|---|---|
| `-data` | `/var/lib/mailgw-go` | SQLite store + identity |
| `-admin` | `0.0.0.0:8080` | admin UI bind address |

**SQLite driver: `modernc.org/sqlite`.** Pure Go, works with `CGO_ENABLED=0`
and `distroless/static` unchanged. It is a large transitive tree (`libc`,
`mathutil`, …) which is a real cost against this module's four-dependency
style, but the alternatives are worse: `mattn/go-sqlite3` needs cgo and a
different base image, and hand-rolling a JSON file gives up atomicity for the
config cache M5 needs. `zombiezen.com/go/sqlite` is a viable swap (also pure
Go, smaller API) if the dependency weight becomes a problem — keep all SQL
behind `internal/store` so the swap stays a one-file change.

> **The admin UI binds `0.0.0.0` by default, and access is controlled by
> firewall.** A wizard reachable only over loopback is not useful on a headless
> server — the whole point is that an operator opens it in a browser to
> provision a box that has nothing on it yet, and requiring an SSH tunnel to do
> that defeats the exercise.
>
> The UI is unauthenticated, so **reachability is the access control**: anyone
> who can open port 8080 can re-point the gateway at another Central Manager and
> have it fetch that manager's configuration. Restrict 8080 to the management
> network at the firewall. Deployment docs must state this plainly — it is a
> deliberate trade, not an oversight. Adding first-run auth to the UI is tracked
> as a follow-up so the firewall stops being the only control.

## Work

### 4.1 `internal/store` — the local SQLite store

New package. Everything that persists on the gateway lives here; no other
package writes SQL.

```go
package store

type Store struct { db *sql.DB }

func Open(dir string) (*Store, error)   // creates dir, opens mailgw.db, migrates
func (s *Store) Close() error

// --- identity -----------------------------------------------------------
type Identity struct {
    GatewayUID  string // assigned by central at registration; "" until then
    PublicKey   []byte // raw 32 bytes
    PrivateKey  ed25519.PrivateKey
    Fingerprint string // sha256 hex of PublicKey
}
func (s *Store) Identity() (*Identity, error)          // generates on first call
func (s *Store) SetGatewayUID(uid string) error

// --- settings -----------------------------------------------------------
func (s *Store) CentralURL() (string, error)
func (s *Store) SetCentralURL(u string) error
func (s *Store) Setting(key string) (string, error)
func (s *Store) SetSetting(key, value string) error

// --- config cache (populated in M5, defined here so the schema is one file)
type CachedConfig struct {
    VersionID  int64
    Version    int
    SHA256     string
    Bundle     []byte
    FetchedAt  time.Time
    AppliedAt  *time.Time
    ApplyError string
}
func (s *Store) SaveConfig(c *CachedConfig) error
func (s *Store) LatestConfig() (*CachedConfig, error)
func (s *Store) AppliedConfig() (*CachedConfig, error)
func (s *Store) MarkApplied(versionID int64, at time.Time) error
func (s *Store) MarkApplyError(versionID int64, err string) error
```

Schema, applied by a tiny `PRAGMA user_version` stepper (no migration library —
this is a single-writer local file, and matching logservice's numbered-files
approach here would be ceremony):

```sql
CREATE TABLE identity (            -- exactly one row, id = 1
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    gateway_uid  TEXT NOT NULL DEFAULT '',
    public_key   BLOB NOT NULL,
    private_key  BLOB NOT NULL,
    fingerprint  TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE config_cache (
    version_id  INTEGER PRIMARY KEY,
    version     INTEGER NOT NULL,
    sha256      TEXT    NOT NULL,
    bundle      BLOB    NOT NULL,
    fetched_at  INTEGER NOT NULL,
    applied_at  INTEGER,
    apply_error TEXT NOT NULL DEFAULT ''
);
```

Notes that matter:

- **`Identity()` generates on first call and is idempotent.** Wrap the
  read-or-create in a transaction; two goroutines racing at boot must not
  produce two keypairs. Store the ed25519 *seed* (32 bytes) or the full private
  key consistently — `ed25519.NewKeyFromSeed` on read is the tidier of the two.
- **File permissions.** `Open` must create the directory `0700` and the database
  `0600`: it holds a private key. In the container the process is uid 65532, so
  the data volume must be a **named volume**, not a host bind — same reasoning
  already written into `docker-compose.yaml` for `mailgw_go_queue`.
- **Busy timeout.** `_pragma=busy_timeout(5000)` and `journal_mode(WAL)` in the
  DSN; the admin UI and the pull loop both touch this file.
- The `config_cache` table keeps **more than one row** so M5's "keep the
  last-good configuration" is a row lookup rather than a re-fetch.

**Tests** (`internal/store/store_test.go`, `t.TempDir()`): identity is stable
across `Open`/`Close`/`Open`; identity generation is idempotent under concurrent
`Identity()` calls; fingerprint matches `sha256(pubkey)` hex; settings
round-trip; a re-opened store keeps the cached config.

### 4.2 `internal/central` — the signing HTTP client

```go
package central

type Client struct {
    BaseURL string
    ID      string             // gateway_uid; empty for Register
    Key     ed25519.PrivateKey
    HTTP    *http.Client       // timeout, default 10s
    Log     *slog.Logger
}

type SystemInfo struct {
    Hostname string   `json:"hostname,omitempty"`
    OS       string   `json:"os,omitempty"`
    Arch     string   `json:"arch,omitempty"`
    CPUs     int      `json:"cpus,omitempty"`
    MemBytes int64    `json:"mem_bytes,omitempty"`
    IPAddrs  []string `json:"ip_addrs,omitempty"`
    Version  string   `json:"version,omitempty"`
}

type RegisterResponse struct {
    GatewayUID  string `json:"gateway_uid"`
    Fingerprint string `json:"fingerprint"`
    Approval    string `json:"approval"`
}
type StatusResponse struct {
    Approval         string `json:"approval"`
    DesiredVersionID *int64 `json:"desired_version_id"`
    DesiredVersion   *int   `json:"desired_version"`
    BundleSHA256     string `json:"bundle_sha256"`
    AppliedVersionID *int64 `json:"applied_version_id"`
}
type ConfigResponse struct {
    VersionID    int64           `json:"version_id"`
    Version      int             `json:"version"`
    BundleSHA256 string          `json:"bundle_sha256"`
    Bundle       json.RawMessage `json:"bundle"`
}
type Report struct {
    AppliedVersionID *int64            `json:"applied_version_id,omitempty"`
    ApplyError       *string           `json:"apply_error,omitempty"`
    RestartRequired  *bool             `json:"restart_required,omitempty"`
    Version          string            `json:"version,omitempty"`
    Metrics          map[string]int64  `json:"metrics,omitempty"`
}

func (c *Client) Register(ctx context.Context, pub []byte, info SystemInfo) (*RegisterResponse, error)
func (c *Client) Status(ctx context.Context) (*StatusResponse, error)
func (c *Client) Config(ctx context.Context) (*ConfigResponse, error)
func (c *Client) Report(ctx context.Context, r Report) error
```

The one function everything depends on being exactly right:

```go
// sign builds the canonical string the console verifies and returns the headers.
//
//   <METHOD>\n<request-target>\n<unix-seconds>\n<sha256-hex of body>
//
// `target` is the path plus query exactly as sent — the console signs
// request.url, which includes the /agent prefix and any query string.
func (c *Client) sign(method, target string, body []byte) http.Header {
    ts := strconv.FormatInt(time.Now().Unix(), 10)
    sum := sha256.Sum256(body)
    canonical := method + "\n" + target + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
    sig := ed25519.Sign(c.Key, []byte(canonical))
    h := http.Header{}
    h.Set("Content-Type", "application/json")
    h.Set("X-GW-Timestamp", ts)
    h.Set("X-GW-Signature", base64.StdEncoding.EncodeToString(sig))
    if c.ID != "" { h.Set("X-GW-Id", c.ID) }
    return h
}
```

- **Send the body bytes you signed.** Marshal once into a `[]byte`, sign that,
  and post that same slice. Re-marshalling between signing and sending is the
  obvious way to break this and it fails as a 401 with no other clue.
- **Set `Content-Type: application/json` even on GET** — harmless, and it keeps
  one code path.
- **Typed errors**, mirroring `webui-fastify/src/logservice.ts`: distinguish
  "console answered non-2xx" (carry the status and body) from "could not reach
  the console". `403` from `/config` is not an error condition — it is "not
  approved yet" and the caller should back off quietly, not log an error every
  poll. `404` from `/config` means "approved, nothing deployed yet"; same.
- **Clock skew is a real failure mode.** A 401 whose body mentions the timestamp
  window should be logged distinctly: "check the gateway's clock" is the fix,
  and it is otherwise indistinguishable from a bad key.

**Tests** (`internal/central/client_test.go`): stand up an
`httptest.Server` that verifies the signature the same way the console does
(`ed25519.Verify` over the recomputed canonical string) and assert each method
sends the right target, headers and body; assert a re-marshalled body fails
verification (the regression guard for the bullet above); assert 403/404 map to
distinguishable typed errors.

### 4.3 `internal/adminui` — the local UI

Stdlib only: `net/http.ServeMux`, `html/template`, `embed.FS`.

```go
package adminui

type Server struct {
    Store    *store.Store
    Version  string
    Spool    *queue.Spool     // for queue depth on the status page; may be nil
    Log      *slog.Logger
    // Hooks injected by main so this package does not import the pull loop.
    Register func(ctx context.Context, centralURL string) error
    State    func() State      // live snapshot: approval, versions, errors
}

func (s *Server) Handler() http.Handler
func (s *Server) ListenAndServe(ctx context.Context, addr string) error  // shuts down on ctx
```

Routes:

| Route | Purpose |
|---|---|
| `GET /` | Wizard when unprovisioned; status page when provisioned |
| `POST /register` | Form post: central URL → validate → `Register` → redirect |
| `GET /healthz` | `200 {"status":"ok"}`, unauthenticated, no DB touch |
| `GET /static/*` | Embedded CSS (one small file, no framework) |

The wizard is one screen: a URL field, a Register button, then the fingerprint
in large monospace with "approve this in the console" and a live-ish
(meta-refresh, 5s) approval state. The status page shows identity + fingerprint,
central URL and approval, config version applied vs desired, `apply_error` if
any, queue depth from `Spool.Len()`, and process/version info.

- **Validate the central URL** before storing: must parse, must be `http`/`https`,
  must have a host. Strip trailing slashes. Reject anything else with the form
  re-rendered and an error — the same shape as the webui's controllers.
- **`html/template` auto-escapes**; do not assemble HTML by concatenation.
- Serve on a plain `http.Server` with `ReadHeaderTimeout` set (gosec/lint
  hygiene, and it is a listener on a real network in the Docker case).

### 4.4 CLI and boot wiring (`cmd/mailgw-go/main.go`)

Add `-data` and `-admin` to `mustParse`. Then `runServe` resolves one of three
modes:

```
1. -config <dir> was given explicitly
   -> today's behaviour, entirely unchanged. No store, no admin UI unless
      -admin was also given. This is what keeps check/explain/testdata/CI and
      the Bun SMTP suite working.

2. managed, unprovisioned  (store has no central URL or no gateway_uid)
   -> open the store, start the admin UI (wizard), do NOT start the SMTP
      listeners and do NOT start the queue runner.

3. managed, provisioned
   -> open the store, start the admin UI (status), and in M4 still refuse to
      serve SMTP because there is no config source yet. M5 turns this into
      "load from the cache and serve".
```

**Not starting SMTP when unprovisioned is deliberate.** A gateway with no
allowlist is a gateway that would deny every peer anyway (the allowlist zero
value denies — `internal/config/allowlist.go`), and starting a listener that
can only reject is worse than not starting it: it looks healthy to a load
balancer. Fail closed, and say so on the status page.

How `-config` is detected as "given explicitly": `flag` has no built-in for
this, so either use `fs.Visit` after `Parse` (walks only flags actually set) or
default `-config` to `""` and treat empty as "managed". `fs.Visit` is less
disruptive — `check`/`explain` keep their `/opt/mailgw-go/config` default.

The admin server starts next to `go runner.Start(ctx)` and shares the same
`ctx`, so the existing `signal.NotifyContext` shutdown path tears it down.

### 4.5 Container and compose

`mailgw-go/Dockerfile`:

```dockerfile
EXPOSE 2525 8080
CMD ["-data", "/var/lib/mailgw-go", "-admin", "0.0.0.0:8080"]
```

`docker-compose.yaml`, `mailgw-go` service:

```yaml
ports:
    - "2525:2525"
    # The admin UI / wizard. Unauthenticated — restrict 8080 to the management
    # network at the firewall.
    - "8080:8080"
volumes:
    - mailgw_go_queue:/opt/mailgw-go/queue
    - mailgw_go_data:/var/lib/mailgw-go   # named volume: the image runs as uid 65532
# the read-only config bind is no longer required in managed mode
```

…plus `mailgw_go_data:` under top-level `volumes:`.

Keep the `./mailgw-go/config:/opt/mailgw-go/config:ro` bind available (commented,
with a note) so file mode remains one edit away for debugging.

## What must not break

- `go run ./cmd/mailgw-go check -config ./testdata/config` — unchanged output.
- `go test -race ./...` including `internal/smtpsrv/contract_test.go`.
- `SMTP_PORT=2525 bun test tests/smtp` against a file-mode binary.
- The uuid hierarchy contract (`X` / `X.1` / `X.1.1`) — untouched by this
  milestone, but the e2e suite is the thing that proves it.

## Verification

```bash
cd mailgw-go
go build ./... && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config      # must still pass

# Fresh gateway, no config at all.
rm -rf /tmp/gw && go run ./cmd/mailgw-go serve -data /tmp/gw -admin 0.0.0.0:8080
```

Then, with the console running (`pnpm certs && docker compose up -d`, migrations
applied, an admin user created):

1. `http://localhost:8080` shows the wizard; SMTP on 2525 is **not** listening
   (`ss -lnt | grep 2525` is empty).
2. Enter the central URL, Register. The page shows a fingerprint.
3. `/gateways` in the console lists it as **pending**, with the *same*
   fingerprint and the systeminfo it declared.
4. Approve it. The gateway's status page flips to approved within one refresh.
5. Restart the gateway. It does **not** re-enter the approval queue (idempotent
   registration by fingerprint) and keeps the same `gateway_uid`.
6. Delete the data volume, restart: a new keypair, a new fingerprint, a new
   pending row. The old row stays until an operator forgets it.
7. Skew a clock (`-timestamp` override in a test, or `date -s`) and confirm the
   401 is logged as a clock problem rather than a key problem.

## Follow-ups this milestone creates

- **The admin UI is unauthenticated and reachable on the network**, so the
  firewall is currently the only control. A first-run "claim" step — a one-time
  code written to the gateway's stdout/log and entered in the wizard, after
  which the UI requires a session — would make the listener safe on its own.
  Worth doing before this ships anywhere untrusted; record it in
  `mailgw-go/TODO.md`. Serving the admin UI over TLS (the `certs/` project
  already issues self-signed pairs) is the natural companion, since the wizard
  posts a central URL and later an operator session over it.
- Re-registration after an operator *forgets* a gateway: the gateway still holds
  a `gateway_uid` that no longer exists, and every signed call 401s. It should
  detect "unknown gateway" and fall back to `Register` with its existing key
  rather than needing its data volume wiped.
- `modernc.org/sqlite` bloats `go.sum` considerably. Check the image size delta
  and record it; if it is objectionable, `zombiezen.com/go/sqlite` is the swap.
