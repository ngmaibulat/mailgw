# Known gaps

Things that are missing or wrong, recorded so they are not rediscovered. The
authoritative lists are `mailgw-go/TODO.md` and `webui-fastify/TODO.md`; this is
the orientation.

## Deliberate, with reasons

These look like omissions and are not — the reasoning is in
[Standing decisions](/architecture/decisions).

- `REQUIRETLS` is not advertised; `mail.requiretls` is declared unpopulated.
- `BINARYMIME` is not enabled, so `mail.body` can never read it.
- Attachment scanning and outbound connection reuse ship **off**.
- The connection pool **refuses** at its global cap rather than evicting, and a
  DNS failure is believed for a fixed 30s. Both are M17 policy decisions.
- The banner greeting is not reproducible — go-smtp owns the string.
- Over-long DATA lines are refused, not folded, because folding breaks DKIM.
- DSN parameters are not propagated to the next hop; the gateway answers for the
  recipients it accepted instead.
- Quarantine release is CLI-only.
- The admin listener is plain HTTP.

## Real gaps in the gateway

**Re-registration after a gateway is forgotten.** The node still holds a
`gateway_uid` the console no longer knows, so every signed call 401s. The error
is already classified distinctly — it should fall back to registering with the
existing key rather than needing its data volume wiped.

**A listener bind failure on the first apply is terminal for that process.**
`smtpListeners` is guarded by a `sync.Once` that has already fired, so a
corrected `listen:` address cannot apply without a restart.

**`check -data` opens the store read-write**, creating the directory, the
database and any pending migration. Running it as a different user than the
gateway leaves stray root-owned WAL files. A `store.OpenReadOnly` would fix it.

**`Backend.Cfg` is captured at bring-up and read live** for the `Received:`
hostname, the event URLs and the attachment scanner — which is why `hostname`,
`logging` and `attach` are on the restart list. An atomic pointer would let all
three hot-swap.

**A managed node's self-signed certificate is not a real certificate.** There is
no ACME client and no way for the console to ship one. An external certbot on the
host works, because renewal in place is picked up without a restart — it is just
not automatic.

**`internal/attach` reads the spooled body back.** Gated so the default costs
nothing, but a deployment with attachment rules pays a second pass over every
message. A streaming walk would remove it; it was rejected as substantially more
failure modes for a cost nobody has measured as a problem. **Measure before
building it.**

**Attachment scanning is one HTTP call inside the DATA reply.** No retries, and a
slow scanner adds its latency to every message before the client is answered. A
per-digest cache would help a fleet sending the same attachment repeatedly.

**A DKIM signing key cannot be distributed from the console.** Only the selector,
the domain and a *path* travel in a bundle; the key itself is a file an operator
places on each edge node, exactly as a real TLS certificate is. This is the rule
rather than an omission — the console keeps every configuration version for ever
and serves it to every gateway on the profile — and it is deliberately not solved
by generating one, because a self-generated key whose public half is not in DNS
produces signatures that *fail*, which is worse than not signing.

**There is no public suffix list**, so DMARC's organizational-domain fallback
walks up exactly one label and relaxed alignment is "equal, or one is a subdomain
of the other". Both err towards reporting `none`/`fail` where a full
implementation reports a pass, never the reverse. Mail from `a.b.example.com`
inherits nothing from `example.com`'s record; `a.example.com` does not align with
`b.example.com`. A PSL would be a megabyte of data and a tenth dependency.

**DKIM verification re-reads the spooled body**, the same cost `internal/attach`
pays and gated the same way. The two passes are independent; folding them into
one walk would halve it, and is worth doing only after somebody measures it.

**Rate limits are per gateway, not per fleet.** Ten edge nodes with a limit of
100/min admit 1000/min collectively, so an operator sizing one has to divide by
the fleet size. Deliberate: a shared counter would put a network round trip in
the accept path and turn a management-plane outage into a mail outage, which is
the exact property `/readyz` was built to avoid.

**The failed-AUTH limiter cannot disconnect.** It answers `454` and the peer may
keep trying on the same connection — every attempt past the limit is refused
without a bcrypt comparison, which is the CPU protection that matters, but the
socket stays open until `inactivity_timeout`. go-smtp's `handleAuth` offers no
hook to close from inside the SASL callback. Feeding a tripped failure budget
into `connect_per_ip`, so the peer's *next* connection is refused, would close
the gap and was left out as too clever for a first cut.

**Nothing rate-limits per relay on the way out.** `outbound.concurrency` and
`per_group_connections` bound connections, not messages per second. A *rate*
there is a question about what receiving relays tolerate, which M7 declined to
guess at and M17 left where M16 put it.

**`max_pooled_connections` is not read live.** `outbound` is on the
`restartRequired` list, so the pool is rebuilt — empty — when the key changes.
Unlike M15's rate limits, which were deliberately made live so an operator can
retune one mid-incident, this is a file-descriptor ceiling and a restart is the
honest cost. It is only worth revisiting if pooling stops being opt-in.

## Real gaps in the console

**The relay routes are not admin-gated.** `/config/relay/*` and
`/config/relaygrp/*` have no `requireAdmin`, so any signed-in viewer can create
or edit a relay including its password — while `roles.ts` says the split exists
precisely so a viewer cannot read relay credentials. This looks like an oversight
rather than a decision.

**There is no fetch timeout on the read proxy.** Errors map correctly to 502 and
504, but a hung log service holds the request.

## Deployment and CI

Out of scope for the code-only milestones, and recorded here so they are not
lost:

- **CI covers the Go module only.** Nothing runs the logservice tests, the
  console checks, or either end-to-end suite. The published workflow builds the
  **legacy Haraka** image, not the gateway.
- **No log rotation anywhere**, and Docker's default driver is unbounded.
- **No healthchecks in compose**, despite the endpoints existing. The runtime
  image has no shell and no `curl`; a `healthcheck` subcommand on the binary is
  the cheap fix. M22 considered exactly that and did not need it — the console
  waits for its own tables at boot rather than for logservice to report healthy
  — so nothing depends on this to come up; it is still worth having for an
  orchestrator that restarts on it.
- **No resource limits** in any compose file.
- **Both compose files pin `:latest`**, so an upgrade is not repeatable — which
  interacts badly with the forward-only store migrations.
- **No backup or restore tooling.** The docs say to back up
  `/opt/mailgw-go/data`; nothing does it, a naive `cp` is unsafe in WAL mode, and
  the **spool** is not mentioned as backup-worthy at all despite holding
  undelivered mail.
- **No alerting on the counters this code documents as alarms.** There is no
  Prometheus, no scrape config and no dashboard anywhere in the repository —
  including for `mailgw_dsn_unroutable_total`, whose own HELP string says any
  non-zero value means senders are not learning their mail failed.

## Credentials to rotate

The committed mailtrap credentials under `deploy/mailgw/settings/` should be
rotated regardless of what else changes.
