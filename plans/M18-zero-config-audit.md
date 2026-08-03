# M18 — Zero configuration, enforced: removing the second source

**Status:** **done** (2026-08-02) · **Packages:** `mailgw-go`, `tests`, `deploy`, `docs`, `webui-fastify` · **Depends on:** M5, M12 · **Blocks:** —

> Read [What was built differently, and why](#what-was-built-differently-and-why)
> before using this as a description of the code. The audit that started this
> milestone returned eleven findings; **four of them were deleted rather than
> fixed**, which is the milestone's actual shape.

## Goal

Make "the gateway is zero-configuration" a property of the code rather than a
property of how it happens to be deployed.

M5 established that a *shipped node* has no environment, no arguments and no
config files, and that held: the image has no `CMD`, and `deploy/gateway` has no
`command:`, no `environment:` and no config mount. But the *binary* still had a
second configuration source beside Central Management — file mode, `-config
<dir>` — protected by a standing decision, "File mode must not regress", because
`check`, `explain`, the sample config directories, the contract suite and the Bun
SMTP e2e all ran on it.

The load-bearing property this milestone adds: **nothing on the host a gateway
runs on can change what it does.** Not a file, not a flag, not an environment
variable.

## Why the second source had to go

An audit against "zero CLI args, zero env reliance" was commissioned before any
of this was planned. It found eleven items. Four of them were not independent
defects at all — they were the *same* defect, a second configuration source,
observed from four directions:

1. **The spool directory resolved differently in each mode.** Managed mode
   substitutes `<data>/queue` when the profile leaves `outbound.spool_dir` at the
   compiled-in default; file mode does not. `deploy/gateway/docker-compose.yaml`
   bind-mounted `/opt/mailgw-go/queue` on both sides, so on every shipped edge
   node that mount was **empty and the real spool was inside the 0700 identity
   directory** — and `02-checkdirs.sh` created and writability-checked a
   directory nothing ever wrote to.

2. **`check`, `explain`, `mailq` and `events` with no flags read a directory,
   while `config show` and `claim` read the store.** `loadFor` branched on
   `o.configSet || !o.dataSet`; `configcmd.go` and `claim.go` branched on
   `o.configSet` alone. So `deploy/gateway/04-gateway.sh` printed a `check`
   command that failed on the node it was printed on — and on a node upgraded
   from file mode, where `/opt/mailgw-go/config` still existed, it *succeeded*
   and validated a configuration the running gateway was not using, while
   `mailq` reported an empty queue.

3. **The two parsers disagreed on purpose**: lax for files so a stray key kept an
   existing deployment booting, strict for bundles because a console textarea is
   exactly where a typo should be caught. `check` had to print which one it had
   used.

4. **`auth_pass_env` authenticated with an empty password.** `Relay.Password()`
   preferred the environment; a managed node has none; `deliver` sent `AUTH
   PLAIN <user> <empty>`. Nothing warned — `PlaintextCredentials` *excluded*
   relays that set the field, so the one configuration that silently broke was
   the one case `gateway.warn` and `check` never mentioned — and `check`'s text
   was `prefer auth_pass_env`, which advises an operator straight into it. The
   console emits the field verbatim and its own comment recommends it too.

None of these is a bug in file mode. Each is a bug in *having two of something*.
Fixing them individually would have left the mechanism that generates them.

## What was removed

- `-config`, `configSet`, `dataSet`, `adminSet`, `load(dir)`, `serveFile`,
  `reloadDir`, and the file branch of `loadFor` / `resolveSpoolDir` /
  `loadForEvents`.
- `config.Load`, `config.LoadAllowlist`, `relays.Load`, `showConfigDir`,
  `Config.Dir`.
- `ParseServer`'s `strict` parameter. One parser, always strict.
- The file-mode admin-UI opt-in, and `TestAuth_FileModeIsUnauthenticated` with
  it.
- Both `os.Getenv` call sites: `events.APIKeyFromEnv` and the `AuthPassEnv` arm
  of `Relay.Password()`.
- `mailgw-go/config/` and `mailgw-go/testdata/config/`.

## What replaced what file mode was carrying

**CI.** The two `check -config` steps over sample directories are replaced by
Go tests over an inline bundle fixture — `internal/config`
`TestBundleConfig_FixtureIsAccepted` and `cmd/mailgw-go`
`TestLoadBundle_FixtureCompiles`. This is strictly better than what it replaced:
it validates the wire format a gateway is actually given. A new CI step greps
for `os.Getenv` in non-test code and fails the build.

**The dev stack.** `docker-compose.yaml` drops `command:`, the config mount and
`API_KEY`; the gateway boots unprovisioned exactly like the production one.

**The e2e suite.** `tests/provision.ts` (`pnpm provision`) drives the console
through the whole provisioning path — first admin, relay group, three profiles,
wait for self-registration, approve the fingerprint, assign, deploy, wait for
SMTP. It is idempotent and the `pnpm test:e2e*` scripts run it first.

It drives **HTML forms**, because the console has no JSON admin API. Seeding
MariaDB directly was the alternative and was rejected: composing a
`ConfigVersions` row by hand means reimplementing `bundle.ts`'s digest and shape
rules in a second place, and the moment those disagree the e2e suite is testing
a bundle no console would produce. That is the same "two sources" mistake this
milestone exists to delete, one layer up.

## `auth_pass_env` is refused, not ignored

Deleting the field would have made a bundle carrying it fall through to
`auth_pass` — which for a relay configured that way is empty, so the silent
empty password would have survived the fix. `NewTable` rejects it by name, with
an error that says what would otherwise go wrong. The bundle is refused whole
and the gateway keeps its last-good configuration, which is the existing
fail-closed contract for a bad bundle.

`TestNewTable_RefusesAuthPassEnv` was confirmed by reverting the check and
watching it fail — the standing rule for this repo.

## One finding fixed on its own: warnings that could not fire

`g.warn()` was called from `bringUp`, and `apply` calls `bringUp` only when
`g.live == nil`; every later apply takes the `swap` path. A gateway changes its
configuration **exclusively** by later applies, so every warning in that
function was structurally unable to fire on the deploy that caused it —
deploying `allow_all: true` logged `configuration applied` and nothing else.

`g.warn(l)` moved to the top of `apply`. It now runs once per applied
configuration, which is once per deploy rather than once per poll.
`TestGatewayApply_WarnsOnEveryApplyNotJustTheFirst` was confirmed by moving the
call back and watching it fail.

This is fixed here rather than deferred because it is one line, and because the
warning it silences is the open-relay one.

## Findings NOT addressed here

These came out of the same audit, survive the removal of file mode, and are
**not fixed**. They are ranked, and the first is the one to do next.

- **The admin bind address has no bundle key.** `config.Admin` carries only
  `metrics_token`, and `-admin ""` is fatal, so a node running `network_mode:
  host` as uid 0 is pinned to `0.0.0.0:8080` and cannot be narrowed from the
  console. The argument against adding one is real — the console could then move
  or disable the UI an operator recovers a node through — so this needs a
  decision, not a patch.
- **`logging.api_key` is absent when the console has no
  `GATEWAY_LOGSERVICE_API_KEY`**, and the logservice URL falls back to
  `http://localhost:3000` fleet-wide. `events.Client.Send` also drops an event
  with an empty URL with no counter, no log and no spill.
- **A DKIM key that disappears after apply stops signing at DEBUG level**, which
  is the outcome `validateDKIM`'s own comment warns about.
- **A missing `central_ca_file` reads as "unprovisioned"** and is not logged.
- **`ProxyFromEnvironment` and `SSL_CERT_FILE`/`SSL_CERT_DIR`** still reach the
  gateway through the stdlib: `internal/central/client.go` clones
  `http.DefaultTransport` and `internal/events/client.go` leaves it nil. So
  "reads zero environment variables" is true of this code and not quite true of
  the process. Setting `Proxy: nil` explicitly is the fix; it was left out
  because it changes behaviour for anyone deliberately running behind a proxy,
  and nobody has been asked.

## What was built differently, and why

**The milestone deleted more than it fixed.** The plan this grew from framed
four of the audit findings as defects to repair. They were repaired by removing
the second configuration source, and the write-up above is deliberately
organised that way — the finding list is a symptom list.

**`check`'s credential warning changed meaning rather than wording.** It said
`plaintext auth_pass in relays.json (prefer auth_pass_env)`. There is no
`relays.json` and no environment, so it now names the relays carrying a password
and says where that password lives — encrypted in the console, decrypted only
when a bundle is composed. `PlaintextCredentials` no longer excludes anything,
which is what makes it able to report the case it used to hide.

**The sample-config tests were deleted, not ported.**
`TestMsgAuth_SampleConfigsSpellItOut` and its rate-limit twin asserted that the
shipped `server.yaml` files mentioned every key, so an operator would find them
by reading the sample rather than the source. There is no sample to read. That
job moved to `docs/public/config/`, and it is worth saying plainly that this is
a **weaker** guarantee: a test enforced it before and prose does not.

**`pnpm start` and `pnpm dev` no longer run a gateway.** They ran the binary
against `mailgw-go/config`. There is no local-files path to offer any more, so
both now bring up the dev stack and provision it. `pnpm check` became
gofmt+vet+test, since there is no configuration on this host to check.

**Two flags survive and are not exceptions.** `-data` is where a configuration
is cached; `-admin` is how a node with no configuration yet is provisioned.
Neither is configuration, and neither can travel in a bundle without a
chicken-and-egg. The same reasoning already applies to the TLS certificate and
the DKIM key: see *A private key never travels in a bundle*.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/mailgw-go serve -config /tmp/x     # must fail: flag not defined
grep -rn 'os.Getenv' --include='*.go' . | grep -v _test.go   # must be empty
```

```bash
docker compose config -q
pnpm docs:build                                 # three sites, no dead links
docker compose up -d && pnpm provision && pnpm test:e2e:smtp
```

The decisive check is that nothing in the repository can hand a gateway a
configuration except Central Management: no directory, no bundle file, no seed
command.
