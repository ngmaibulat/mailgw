# M5 — Config pull, SQLite cache, versioned apply and rollback

**Status:** **done** (2026-07-29)  ·  **Packages:** `mailgw-go`, `webui-fastify`, `logservice/migrations`, `deploy`  ·  **Depends on:** M4 (done)  ·  **Blocks:** M6

> Read **[What was built differently, and why](#what-was-built-differently-and-why)**
> at the end before using this document as a description of the code. The scope
> grew: the shipped node is now zero-configuration, which moved three things out
> of the gateway's environment and into what Central Management serves, and added
> a WebSocket notification channel that is not described below.

Read [README.md](./README.md) for the bundle format and signing contract.

## Goal

A managed gateway loads its entire configuration from the bundle Central
Management deployed to it, cached locally in SQLite so a console outage is a
non-event. Deploy and rollback both work end to end, a bad bundle never takes
the gateway down, and file mode still works exactly as before.

The load-bearing property: **the gateway is authoritative for validation.** The
console does shape checks only. A bundle that does not compile must leave the
running configuration untouched and surface the compiler's message in the
console.

## The shape of the change

`load(dir)` at `cmd/mailgw-go/main.go:103` is already the single chokepoint used
identically by `serve`, `check` and `explain`. That is the whole reason this is
tractable: make it source-agnostic and everything downstream follows.

```go
// Source produces a validated, compiled configuration. FileSource is today's
// behaviour; CentralSource reads the SQLite cache M4 fills.
type Source interface {
    Load() (*loaded, error)
    Describe() string          // for logs and `check` output
}

type FileSource   struct{ Dir string }
type CentralSource struct{ Store *store.Store }
```

`loaded` (`main.go:92`) stays exactly as it is — `{cfg, file, rules, source, legacy}`.

## Work

### 5.1 Byte-slice parse entry points

Every loader today is `os.ReadFile` + parse. Split the parse half out. **Do not
duplicate validation** — factor it so the file path calls the byte-slice path,
which is the pattern `relays.NewTable` already sets ("Separate from Load so that
tests, and any future configuration source, get exactly the same validation").

| Existing | Add | Notes |
|---|---|---|
| `ruleset.LoadFile(path)` — `ast.go:142` | `ruleset.ParseFile(raw []byte, name string) (*File, error)` | Keep `yaml.UnmarshalStrict` and the `version` check. `name` is only for error messages. |
| `config.LoadAllowlist(path)` — `allowlist.go:33` | `config.ParseAllowlist(raw []byte, name string) (*Allowlist, error)` | **Must keep returning a deny-all `*Allowlist` alongside every error** — that fail-closed contract is asserted by `allowlist_test.go` and mirrors `npFilter.js:52-57`. |
| `config.Load(dir)` — `config.go:224` | `config.ParseServer(raw []byte) (Server, error)` | Start from `defaults()`, unmarshal over it, then `validate()`. See the strictness note below. |
| `relays.Load(path)` | *nothing* — `relays.NewTable(map[string][]Relay)` (`relays.go:135`) already exists | Unmarshal the bundle's `relays` object into `map[string][]relays.Relay` and hand it over. `Relay.Port` accepts a JSON number or string, so the bundle's number form works unchanged. |

`ruleset.Compile` (`eval.go:93`) already takes a `*File` and never touches the
filesystem, so the compiled artifact needs no change at all.

**One asymmetry to resolve deliberately.** `routing.yaml` is parsed with
`yaml.UnmarshalStrict` (an unknown key is an error — a misspelled `piority:`
would silently reorder the table), but `server.yaml` uses non-strict
`yaml.Unmarshal`. A bundle arriving from a console is machine-generated from a
textarea an operator typed into, which is exactly the case strictness is for:
**parse both strictly on the central path.** Keep `config.Load`'s file path
non-strict so existing deployments with stray keys still boot; add
`ParseServerStrict` or a `strict bool` and use it only from `CentralSource`.
Note this divergence in `check` output so it is not a surprise.

### 5.2 The bundle type

```go
// internal/config/bundle.go — mirrors webui-fastify/src/central/bundle.ts
type Bundle struct {
    Format    int                        `json:"format"`
    Server    *string                    `json:"server"`
    Routing   *string                    `json:"routing"`
    Allowlist json.RawMessage            `json:"allowlist"`  // {"allowed":[...]}
    Relays    map[string][]relays.Relay  `json:"relays"`
    Logging   Logging                    `json:"logging"`
}
```

- Reject `Format != 1` with a clear message. The console will bump it if the
  shape ever changes, and a gateway that guesses is worse than one that refuses.
- `Allowlist` stays `json.RawMessage` so it can be fed straight to
  `ParseAllowlist` — reusing the exact validator, including the "empty allowlist
  requires `allow_all: true`" rule.
- A `nil` `Routing` is not an error at parse time but is a gateway with no
  routes; `Compile` + `default_action` already handle "no route found". Warn.
- A `nil` `Server` means fall back to `defaults()` — a working configuration for
  everything except the relay table, which is what `config.Load` already does
  when `server.yaml` is absent.

### 5.3 The pull loop

A goroutine started next to `watchReload` in `runServe`, sharing its `ctx`.

```
every poll_interval (default 30s, jittered ±10% so a fleet doesn't stampede):
  status, err := central.Status(ctx)
  err != nil                    -> log at WARN, keep running, back off
  approval != "approved"        -> log at INFO once per transition, do nothing
  desired_version_id == applied -> nothing to do
  otherwise:
    cfg, err := central.Config(ctx)
    404 -> approved but nothing deployed; nothing to do
    store.SaveConfig(...)               // cache BEFORE attempting to apply
    err := apply(cfg)
    ok   -> store.MarkApplied(...);  central.Report{applied_version_id, apply_error: null}
    fail -> store.MarkApplyError(...); central.Report{apply_error: <compiler message>}
            and KEEP RUNNING THE PREVIOUS CONFIGURATION
```

Reuse `watchReload`'s semantics rather than inventing new ones
(`cmd/mailgw-go/main.go:429-476`):

- **All-or-nothing.** Parse and compile everything, then swap. A failure at any
  step leaves both `atomic.Pointer`s untouched.
- **Only allowlist + rules hot-swap.** `validateAgainstLiveRelays` (`main.go:473`)
  already exists and recompiles the new rules against the **live** relay table
  precisely so a rule can never name a group the runner cannot dial. Keep it.
- **Everything else needs a restart.** Detect and report rather than half-apply:

  ```go
  // restartRequired compares the incoming bundle against what the process
  // actually started with. The runner holds its relay table for the life of the
  // process, and listeners/TLS/spool are bound at startup.
  func restartRequired(live *config.Config, next *config.Config) []string
  //   -> ["relays", "listen", "tls", "spool_dir", "outbound"] etc.
  ```

  Apply what can be applied (allowlist + rules), set `restart_required: true` in
  the report with the list of what changed, and log it at WARN. The console
  already renders this as a banner on the gateway detail page.

- **`SIGHUP`** keeps meaning "re-read the files" in file mode, and becomes
  "re-apply from the cache" in managed mode (useful after a manual `restart`,
  and it makes the two modes feel the same).

**Rollback needs no gateway-side code.** The console repoints
`desired_version_id` at an older row; the loop sees a different desired version
and pulls it. That is the whole feature, and it is why bundles are immutable.

### 5.4 Boot in managed mode

`CentralSource.Load()` reads the **applied** cached config if there is one, else
the latest cached. Concretely, boot order becomes:

```
open store
if unprovisioned            -> wizard only, no SMTP (M4 behaviour)
if provisioned, no cache    -> admin UI + status; SMTP does not start;
                               the pull loop runs and will start SMTP once a
                               bundle applies
if provisioned, cache hit   -> load from cache, start SMTP, then start the pull
                               loop (which may immediately upgrade the config)
```

The gateway **must be able to boot with the console unreachable** — that is the
point of the cache. Do not make startup block on a successful poll.

Starting the SMTP listeners *after* first successful apply (rather than at
process start) is the one structural change to `runServe`: extract the
"create listeners, guard, serve" block into a function that can be called once,
guarded by a `sync.Once`, from either the boot path or the pull loop.

### 5.5 `check` and `explain` in managed mode

- `mailgw-go check -data <dir>` should validate the **cached** bundle and print
  the same summary as file mode, so "is what this gateway is running actually
  valid?" is answerable on the box.
- Add `mailgw-go config show -data <dir>` printing the cached bundle (with
  `auth_pass` redacted) — the operator equivalent of `cat routing.yaml`, and
  the first thing anyone will want when a route misbehaves.
- `explain` should work against the cached ruleset too; it is the single most
  useful debugging tool in the product and it currently needs a config directory.

### 5.6 `auth_pass` encryption at rest

Stage 5 of the old bridge epic, unblocked because the bundle is now a real
decrypting consumer.

- AES-256-GCM, key from `CONFIG_SECRET_KEY` on the **console** side
  (`webui-fastify`), decrypted where? Two options, and the choice matters:
  1. **Console decrypts before composing** — the bundle carries plaintext over
     TLS. Simple; the secret never leaves the console; the bundle at rest in the
     gateway's SQLite is plaintext.
  2. **Gateway decrypts** — the bundle carries ciphertext, and every gateway
     needs the key, which makes the key a fleet-wide shared secret. Worse.

  Take (1), and note that the real fix for at-rest exposure on the gateway is
  `auth_pass_env` (already supported by `relays.Relay.Password()`), which keeps
  the credential out of the bundle entirely. Consider making the console emit
  `auth_pass_env` references for gateways that declare they can provide them.

This is the lowest-priority item in the milestone; ship the pull loop first.

## What must not break

- `-config <dir>` file mode, byte for byte. `FileSource` is a wrapper around
  the existing `load(dir)` body; if that function changes behaviour the whole
  contract suite should fail, and if it doesn't, add a test that would.
- `go test -race ./...`, `internal/smtpsrv/contract_test.go`, and
  `SMTP_PORT=2525 bun test tests/smtp` against a file-mode binary.
- `internal/ruleset/transpile_test.go` — the legacy `routing.json` equivalence
  proof. Refactoring `LoadFile` must not touch it.

## Tests

- `internal/config`: `ParseAllowlist` returns deny-all on every malformed input
  (port the existing `allowlist_test.go` table to the byte-slice entry point and
  have the file test call through it); `ParseServer` defaults + validate;
  strict-mode rejects an unknown key.
- `internal/ruleset`: `ParseFile` accepts what `LoadFile` accepts for the same
  bytes — assert by running both over `testdata/config/routing.yaml`.
- `internal/config/bundle_test.go`: a golden bundle (copy one out of a real
  `GET /agent/config`) parses into the same `*loaded` a file-mode load of
  `testdata/config` produces. **This is the highest-value test in the
  milestone** — it is the actual claim being made.
- Pull loop, with a fake `central.Client`: applies a new version; a bad ruleset
  leaves the previous `atomic.Pointer` values in place and reports `apply_error`;
  a relay change sets `restart_required` and does not swap the relay table; a
  console outage is a WARN and no state change; rollback to an older version
  applies it.
- `restartRequired` unit table.

## Verification

```bash
cd mailgw-go && go build ./... && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config     # unchanged
pnpm test:mailgw-go && SMTP_PORT=2525 bun test tests/smtp  # file mode intact
```

End to end, against a running console with an approved gateway:

1. In the console create a `ruleset` profile (paste `testdata/config/routing.yaml`),
   an `allowlist` profile, assign them plus a relay group, press **Deploy**.
2. Within one poll the gateway shows applied **v1**, and SMTP starts listening
   on 2525.
3. `bun test tests/smtp` passes against it — the deployed ruleset routes mail.
4. Edit the ruleset, Deploy **v2**: applied within one poll, **no restart**,
   and mail keeps flowing throughout (send during the swap).
5. **Roll back to v1**: the gateway returns to v1 and the digest matches v1's
   exactly.
6. Deploy a deliberately broken ruleset (`priority: "high"`, or a field name
   that is not in the registry). The gateway **keeps running v1**, and the
   console shows the compiler's message in `apply_error`.
7. Change a relay host and Deploy. The console shows `restart_required`; rules
   and allowlist still applied; the relay table did not change until restart.
8. Stop the console. Restart the gateway. It boots from the cache and serves
   mail. Start the console again; it reconciles without operator action.

---

## What was built differently, and why

Seven deliberate deviations from the plan above, all found while implementing it.

**The node became fully managed, which was not in scope here.** The decision
taken during implementation was that a shipped gateway has **no environment
variables, no CLI arguments and no configuration files on the host** — it boots
empty, is provisioned through the wizard, and takes everything else from Central
Management. That forced three things out of the gateway and into the bundle
(§5.7 below), made the admin UI a permanent listener rather than an opt-in one,
and turned `deploy/gateway` into a compose file with no `command:` and no
`environment:` at all. `-config <dir>` survived as a CLI capability at the time,
because `check`, `explain`, `testdata/config`, the contract suite and the Bun
SMTP e2e all ran on it.

> **Superseded (M18).** That second configuration source is gone: `-config`, the
> directory loader, the sample config directories and every environment variable
> the gateway read have been removed, and the standing decision "File mode must
> not regress" was reversed. Four defects came out with it, including a relay
> `auth_pass_env` that authenticated with an empty password. See
> [M18](./M18-zero-config-audit.md) and
> `docs/internal/architecture/decisions.md`.

**The `Source` interface was dropped.** `Load() (*loaded, error)` has nowhere to
carry the version id or the `MarkApplied`/`MarkApplyError` bookkeeping the
central path needs, and `CentralSource` would have needed a gateway to apply to.
Two free functions returning the same type — `load(dir)` and `loadBundle(raw,
opts, source)` — give the same source-agnosticism with no interface to keep
honest. `Describe()` is the `loaded.source` string that `check` already printed.

**Boot follows recorded intent, not a timestamp.** §5.4's "the applied cached
config, else the latest" is wrong in two directions, and both were caught by
tests rather than by reading. `applied_at` and `fetched_at` have one-second
resolution, and a rollback lands in the same second as the deploy it undoes, so
either ordering tie-breaks on `version_id` — which is precisely the bundle the
operator just rejected. Two fixes: a new `applied_seq` column (store migration 2)
makes "most recently applied" answerable, and a new `desired_version_id` setting
records what the console last asked for, which is what boot actually reads.
Without the second, a restart-required deploy would restart into the *old*
bundle and the operator's action would do nothing.

**`MarkApplied` fires on a partial apply too**, for the same reason: boot reads
the applied row.

**`Report.RestartRequired` is always sent.** It carries `omitempty` and the
console merges on `!== undefined`, so a nil pointer could never clear a stale
`restart_required: true` — it would haunt the gateway's row for the life of the
process. `apply_error` is truncated to 4000 characters, because the console's
schema rejects anything over 4096 and a rejected report loses the heartbeat as
well as the error.

**§5.6 took option (1), as recommended** — the console decrypts before composing
— but the migration it needed was bigger than "encrypt a column". A managed
gateway with no environment cannot use `auth_pass_env`, cannot be told to require
TLS to a relay, and cannot authenticate to logservice, so migration 022 adds
`tls`, `allow_insecure_auth` and `auth_pass_env` to `Relays`, and the bundle
gained `logging.api_key` and `allowlist.allow_all`. That last one was a real
hole: the console stripped `allow_all`, so an allow-all gateway was unreachable
from the UI while the gateway's own error message advised setting it.

**A WebSocket notification channel was added (not in the original plan).**
`GET /agent/ws`, authenticated by the same Ed25519 signature over the upgrade
request, so it introduces no new authentication. Frames carry no state: the
gateway is told "go and look" and asks `/agent/status` what changed, which makes
a duplicated or delayed notification harmless. The 15-second poll is untouched
and still does the same job underneath — a socket that never connects costs only
latency. Two consequences worth knowing: the Go dialer must use an **HTTP/1.1**
transport (`ws` speaks the HTTP/1.1 upgrade, not RFC 8441 over h2, which is why
the console's `allowHTTP1: true` matters), and a second console replica's deploy
does not reach this process's in-memory bus, so each live socket also re-reads
its own gateway row every 10 seconds as a backstop.

### Accepted risk: the admin UI

Making the wizard the only provisioning path means it is always listening,
unauthenticated, on a root process on an internet-facing relay. Anyone who
reaches port 8080 can re-point a gateway at a hostile Central Manager and be
handed its relay credentials. `deploy/gateway/05-firewall.sh` is now **required
rather than optional** and is referenced from `04-gateway.sh`'s output. A
first-boot claim code remains the tracked follow-up that would make the listener
safe on its own.
