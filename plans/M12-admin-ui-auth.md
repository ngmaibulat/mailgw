# M12 — Admin UI authentication

**Status:** **done**  ·  **Packages:** `mailgw-go/internal/{adminui,store,config}`, `cmd/mailgw-go`, `webui-fastify`, `deploy`  ·  **Depends on:** M4, M5  ·  **Blocks:** console-side quarantine release

> Source: `mailgw-go/TODO.md:129-136`, which called this **"the highest-priority
> item in this file, not a nicety"**, plus `plans/M5-config-pull.md:325-333`
> ("Accepted risk: the admin UI") and `plans/M4-local-admin-wizard.md:39-50`.
>
> Numbered M12 but worked **fourth**, after M10, M11 and M16. The number is
> identity; running order lives in `plans/README.md`.

## Why this is the ranked item

M4 designed the admin UI as *"reachability is the access control"* — a
deliberate trade, documented at `internal/adminui/server.go:8-16`, and defensible
while the UI was opt-in.

M5 changed the premise. Making the wizard the only provisioning path means the
UI is **always listening**, and managed mode explicitly refuses to start without
it (`cmd/mailgw-go/main.go:477-481`). On an edge node that is:

- an **unauthenticated** HTTP server,
- bound to `0.0.0.0:8080` by default (`cmd/mailgw-go/main.go:106`),
- in a container with `network_mode: host` and `user: "0:0"`
  (`deploy/gateway/docker-compose.yaml:36,41`) — so there is no port mapping to
  narrow and the process is root,
- on an internet-facing relay.

`POST /register` (`internal/adminui/handlers.go:129-183`) reads form values
directly, with **no CSRF token** and no session. Anyone who reaches port 8080
can re-point the gateway at a Central Manager they control, which then hands it
a full routing configuration — **including relay credentials**, which the console
decrypts before composing the bundle precisely so they travel in cleartext
(`plans/M5-config-pull.md:185-201`). The same form can set `InsecureSkipVerify`
on the management channel (`handlers.go:122` → `internal/central/client.go:98-100`).

`deploy/gateway/05-firewall.sh` is the only control today, which is why
`deploy/README.md:119-139` documents it as required rather than optional. This
milestone makes it defence in depth instead of the whole defence.

It also unblocks two things held behind it:

- **Console-side quarantine release** (`mailgw-go/TODO.md:287-294`) — deliberately
  not built because *"a state-changing unauthenticated POST that releases held
  mail is worse than the gap it closes."*
- **Any auth on `/metrics`** (`plans/M6-observability.md:247-253,330-335`), which
  shares this listener.

## M12.1 — first-boot claim code

The fix the docs already propose (`TODO.md:129-136`,
`plans/M4-local-admin-wizard.md:419-426`).

On first boot, generate a one-time code, persist it in the store beside the
Ed25519 identity, and write it to the log — `docker logs` is reachable by whoever
provisions the host and by nobody else. The wizard refuses every state-changing
action until the code is presented; presenting it establishes a session and
consumes the code.

Design points:

- **It lives in `internal/store`**, in the same `0700` directory and `0600`
  database as the identity (`internal/store/store.go:81,88,93-101`). It is
  therefore covered by the backup story that already exists for
  `/opt/mailgw-go/data`.
- **A claimed node stays claimed.** Re-provisioning to a different console is a
  logged-in action, not a second claim — otherwise the window reopens on every
  reconfiguration.
- **File mode is unaffected.** There, `-admin` is opt-in and off by default
  (`cmd/mailgw-go/main.go:594`), and the operator already has the filesystem.
  Do not make `check`, `explain` or the contract suite need a credential.
- **Recovery must exist and must be documented**: an operator who loses the code
  needs a path that is not "wipe the data volume", because wiping it destroys
  the identity and forces re-approval (`deploy/README.md:141-147`). A CLI
  subcommand that mints a new code, runnable only by someone with filesystem
  access, is the natural answer — filesystem access is already game over.

## M12.2 — session and CSRF on every state-changing POST

Once claimed, the UI carries a session cookie and every mutating handler
requires a CSRF token. `internal/adminui` is stdlib-only
(`net/http.ServeMux` + `html/template` + `embed`, per the constraint in
`plans/README.md`) and this does not change that: a signed cookie and a
per-session token are a few dozen lines.

`POST /register` is the one that matters, but audit `internal/adminui/handlers.go`
for the full set rather than assuming it is alone.

## M12.3 — bearer token for `/metrics` and `/readyz`

`/metrics`, `/readyz` and `/healthz` all sit on this listener
(`internal/adminui/server.go:116-118`). The exposition carries traffic volumes,
queue depth and — via `mailgw_build_info` — the running version
(`internal/adminui/observe.go:83-88`).

The design constraint is recorded at `plans/M6-observability.md:247-253` and is
the reason this cannot simply inherit M12.2: **a Prometheus scraper needs a
credential it can present non-interactively — a bearer token, not a session
cookie.** So:

- `/metrics` and `/readyz`: a bearer token, delivered in the config bundle so a
  managed node has no environment to read it from, and **omitted when empty** so
  an unchanged bundle keeps its digest. Empty means open, preserving today's
  behaviour for anyone who has firewalled the port and does not want to
  re-plumb their scraper.
- `/healthz` stays open. It is liveness-only and performs no I/O; a container
  runtime probing it must not need a secret.

## What this does not change

- **The listener still binds `0.0.0.0` by default** and managed mode still
  requires it. Authentication is the fix, not hiding.
- **`deploy/gateway/05-firewall.sh` remains required.** Update
  `deploy/README.md` and the comments in `internal/adminui/server.go:8-16` and
  `mailgw-go/TODO.md` to say *why* it is still required once auth exists —
  reducing attack surface on a root process is worth doing even when the surface
  is authenticated.
- **Serving the admin UI over TLS** is the natural companion (the wizard posts a
  central URL and now a claim code over it) and `internal/tlsx` already generates
  a self-signed pair for the SMTP side. Decide it in this milestone rather than
  leaving it implied: a claim code posted in cleartext over a management LAN is
  a smaller problem than the one being fixed, but it is not nothing.

## Verification

```bash
cd mailgw-go
gofmt -l . && go vet ./... && go test -race ./...
```

`internal/adminui` already has `server_test.go` and `observe_test.go` (22 tests).
Add:

- an unclaimed node refuses `POST /register` and every other mutating handler;
- the code works exactly once, and a second presentation fails;
- a claimed node refuses a POST with a missing or wrong CSRF token;
- `/metrics` and `/readyz` refuse a wrong bearer token and accept the right one,
  and both are open when no token is configured;
- `/healthz` is open in every state.

End-to-end, the wizard flow in `deploy/gateway/04-gateway.sh` must still work:
the script prints the wizard URL, and it now needs to print (or point at) the
claim code as well.

---

## What was built differently

Seven things. The first is a correction to this plan rather than an
elaboration of it.

**1. The claim code is NOT consumed, and this plan was wrong to say it should
be.** M12.1 above says presenting it "establishes a session and consumes the
code". Take that literally alongside a cookie as the only other credential and
the node ends up reachable by exactly one browser for ever: a second operator, a
cleared cookie, a new laptop or a browser profile switch each need
`docker exec … claim reset`, which also signs out everybody else. Re-read lines
66-67 — *"a claimed node stays claimed… otherwise the window reopens on every
reconfiguration"*. What must not reopen is the **unauthenticated** window, and
that closes the moment a code exists, not the moment one is spent.

So the code is the node's admin secret and `admin_claimed_at` records only its
first use. Its one job is to stop a live credential being echoed into
`docker logs` on every subsequent boot; while unclaimed it is re-logged every
start, which is what an operator who restarts the container before provisioning
it needs.

That also settles the plaintext-versus-hash question more sharply than "hashing
buys little": plaintext is what lets `mailgw-go claim status` answer *"I lost the
code"* **without rotating it**, so an operator recovering their own access does
not sign every colleague out. The threat a hash addresses — the database leaks
but the process does not — is empty here, because the same 0600 file holds the
Ed25519 seed, and whoever has that re-registers as this gateway and is handed the
relay credentials by the console directly.

**2. Sessions live in SQLite, not in the process.** Three reasons, and the first
is the one that decided it: a restart is *most likely during provisioning* — a
bundle applies with `restart_required` and the operator restarts — which is the
worst available moment to drop them back to the sign-in page. Second, the
per-session CSRF token has to survive with its session, or a form rendered
before a restart fails inscrutably when submitted after it. Third, `claim reset`
needs somewhere to revoke *every* session at once, and `DELETE FROM
admin_sessions` is the honest implementation. Store migration 3.

Nothing is cached in memory on `*Server`, deliberately: a cached session would
keep being honoured after `claim reset` run from a second process had revoked
it. `Handler()` is rebuilt on every call (the existing tests rely on it), so
keeping all state in the store also means there is nothing there to get wrong.

**3. The whole UI needs a session, not only the mutating POSTs.** M12.2 asks
only for the latter, but the status page carries the console URL, the approval
state, the console's own error text and the queue depths — reconnaissance this
milestone's threat model cares about. An unauthenticated visitor now gets one
page: version, fingerprint, and a field for the code.

The fingerprint stays visible on it. That is a product property M4 built
deliberately — it exists before registration precisely so a gateway can be
pre-approved — and a bare login page would have destroyed it silently. It is
`sha256(public key)`, the console publishes it for approval anyway, and
registration proves possession of the private half, so knowing it grants
nothing.

`renderClaim` builds its own three fields rather than calling `baseData()`, for
a second reason beyond the leak: `baseData` calls `Spool.LenAll`, which stats
several directories, and an unauthenticated caller must not be able to ask for
that in a loop.

**4. The claim throttle is in memory and never touches the store.** The obvious
implementation — an attempt counter in `settings` — would make `POST /claim` an
*unauthenticated write* to the SQLite file the poll loop and every page view
share: a denial-of-service primitive this process did not previously have,
bought against a brute-force attack that 100 bits of Crockford base32 already
makes impossible. What the bucket (5 tokens, 1/s) is actually for is stopping a
scanner filling the log. It refuses fast and never sleeps: sleeping on failure
inside the 30-second `ReadTimeout` would let an attacker park goroutines until a
real operator's claim queued behind them, turning the throttle into the outage
it exists to prevent.

**5. Middleware is wrapped per route, not around the mux**, so `Handler()` reads
as the access-control policy. A route added in M13 that forgets `s.session` is a
visible omission in a ten-line table; the same route under a middleware that
decides by path prefix would be an invisible hole. `/` is session-*aware* rather
than session-gated — with no session it renders the claim page, because a 403
there would leave an operator with nowhere to sign in.

**6. Cookie details that look like omissions and are not.** `Secure` is set iff
`r.TLS != nil`: setting it unconditionally over this plain-HTTP listener would
make the browser refuse to store the cookie at all, and the UI would silently
never sign anyone in. `SameSite=Lax` rather than Strict, because every mutating
route is a POST carrying a per-session token — Strict would add only protection
against top-level GET navigation, and would cost one baffling report where an
operator following a link from another origin lands on the claim page as though
they had never signed in. No `Max-Age`, so the server-side `expires_at` (12h) is
the only thing deciding how long a session really lives.

**7. `admin` is a bundle key with a config-directory twin, and the token is read
live.** `admin.json` / the bundle's `admin` object keeps the "bundle keys are
this directory's files one for one" invariant literal, and — unlike putting the
token in the `server` profile text, which `RedactBundle` cannot reach into —
lets `config show` redact it beside `logging.api_key`. It lands on
`config.Config.Admin` rather than `Config.Server`, because `bundle_test.go`
asserts a bundle and a directory produce an identical `Server`.

The gateway reads it from an `atomic.Pointer[string]` set on every successful
apply, never from `g.live`, which would take `g.mu` on a scrape. That puts it in
a **third category** beside "hot-swapped" and "needs a restart": it is in force
before `apply` returns, so it is deliberately absent from `restartRequired`'s
list. Nothing reflects over that list, so the only guard is the comment there.
A failed apply keeps the last-good token, or a bad deploy could silently unlock
`/metrics`.

One window cannot be closed: `serveManaged` starts the UI before
`bootFromCache`, so `/metrics` is open for a few milliseconds at process start.
The token is in a configuration the process has not read yet, and double-parsing
the bundle to close it is not worth it.

**And the thing this milestone had to decide:** the admin UI **stays plain
HTTP**. Serving it with the self-signed pair `internal/tlsx` already generates
would authenticate nothing, and would teach an operator to click through a
browser warning on the exact page where they type a secret — which is precisely
the click a MITM needs. It would also invalidate the `http://…:8080` URL
`04-gateway.sh` prints, every operator bookmark and any probe. The follow-up is
tracked concretely as `-admin-tls <cert>,<key>`, real certificate paths on the
gateway's own filesystem and **never** in a bundle (the console stores versions
forever and serves them to every gateway on the profile).

### Upgrading an existing node

A gateway already in the field has no `admin_claimed_at`, so it comes up
unclaimed, mints a code and logs it. Its operator reads it once from
`docker logs mailgw-go`, `mailgw-go claim status`, or by re-running
`04-gateway.sh`, which now prints it.

Back-filling "claimed" for any node that already has a `gateway_uid` was
rejected: it would either leave the UI open, which is the whole defect, or mark
it claimed with nobody holding a session and force `claim reset` anyway.

### Tests

`internal/store/admin_test.go` (13) and `internal/adminui/auth_test.go` (16)
are new; ten existing `server_test.go` tests gained `signIn`/`getAs`/
`postFormAs`, which is cheap only because that file is `package adminui` and can
mint a session directly — no test-only exported API. `observe_test.go` needed no
change: it builds `&Server{}` with a nil `MetricsToken`, so both endpoints stay
open, which is itself the assertion that unset means open.

`cmd/mailgw-go/gateway_test.go` gains two, and they exist because of M16's
lesson: `internal/adminui`'s own tests hand it a `MetricsToken` closure directly,
so nothing there would notice if the bundle field never reached the gateway.
They assert the token follows a bundle through `bringUp` *and* through a later
`swap`, that removing the key reopens the endpoints, and that a **failed** apply
keeps the last-good token. Both were confirmed by reverting the fix.

## Deliberately not done here

- **Console-side quarantine release.** The objection this milestone existed to
  remove is gone — a release button on the *local* admin UI is now an ordinary
  session-and-CSRF-protected POST. A **console** button still needs a
  console→gateway command channel that does not exist; config flows one way, as
  bundles. Separate work.
- **Serving the admin UI over TLS.** Decided against above, tracked as
  `-admin-tls`.
- **A token for `/metrics` in file mode from anywhere but `admin.json`.** File
  mode has no console, and `-admin` there is opt-in behind a flag.
- **Rate-limiting anything but `POST /claim`.** The other routes are behind a
  session.
- **Rate-limiting or nonce-ing `/agent/register` on the console side**
  (`plans/M9-*.md:280-284`). Console-side, `webui-fastify`, and unrelated to the
  gateway's own admin listener.
- **Removing `central_insecure_tls`.** The console ships a self-signed pair, so
  the checkbox is load-bearing until that changes. Authenticating the wizard
  means only an operator can set it.
