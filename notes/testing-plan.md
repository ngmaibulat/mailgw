# Testing plan — 2026-07-29 milestones

Covers the two things that landed today: the Go gateway and Central Management.
High-level items only — what to verify, not how.

## mailgw-go (Golang gateway)

- `check` rejects a bad config, accepts the shipped ones
- Rule matching: every operator, `all`/`any`/`not`/`every`
- Stage inference — a rcpt-only reject fires at `RCPT TO`, not at DATA
- Glob dialects: dot-stopping for domains, dot-crossing elsewhere
- `routing.json` → transpiled ruleset behaves like the legacy matcher
- Per-recipient routing and the envelope split by relay group
- Relay selection, auth, and failover to the next relay
- Spool durability: crash/restart recovery, retry backoff, `dead/`
- IP allowlist is fail-closed (missing/malformed file denies all)
- `SIGHUP` reload is all-or-nothing; a reload needing a restart is refused
- UUID hierarchy `X` / `X.1` / `X.1.1` across the three log tables
- The `250 ... queued (<txn>)` reply contract
- Audit events reach logservice; failures spill to `failed-events/`
- `explain` and `fields` output

## Central Management (webui-fastify)

- First-boot registration lands `pending`; re-register is idempotent
- Fingerprint approve / reject / revoke
- Signature verification: bad signature, clock skew, tampered body
- `GET /agent/config` is 403 until approved
- Bundle keys and shapes match the config-directory files
- Deploy freezes an immutable version row
- Rollback repoints at an old version — byte-identical config
- Redeploying an unchanged config re-points instead of adding a version
- Config profile CRUD; allowlist profile shape is validated
- Admin vs viewer: approval, deploy, and config writes are admin-only
- Sessions survive a webui restart
- `/agent/*` is outside the cookie gate and the audit log

## Cross-cutting

- Bun SMTP e2e suite against the running container
- Read proxy error mapping: 502 upstream, 504 unreachable
- Delivery schema accepts null sender, non-FQDN hosts, IPv6
- Full `docker compose up` stack smoke
- Haraka parity run (uncomment the legacy service, move go off port 25)
