# Release checklist

What to run before shipping, and what to record.

## Automated, first

Nothing manual is worth starting until these are green.

```bash
cd mailgw-go
gofmt -l .                     # must print nothing
go vet ./...
go test -race ./...
go run ./cmd/mailgw-go check -config ./testdata/config

cd ../logservice && bun test tests/
cd ../webui-fastify && pnpm typecheck && pnpm check && pnpm test
```

Then, against a running stack:

```bash
pnpm test:e2e:smtp
pnpm test:e2e:api
```

::: warning CI does not run most of this
The pipeline covers the Go module only. Nothing runs the logservice tests, the
console checks, or either end-to-end suite — so on a release, run them yourself.
:::

## Manual, by scope of change

### Always

- [ ] [TP-01 · Smoke test](/plans/tp-01-smoke)
- [ ] [TP-10 · Observability](/plans/tp-10-observability), steps 1–5 and 13–16

### If the SMTP path changed

- [ ] [TP-02 · IP allowlist](/plans/tp-02-allowlist)
- [ ] [TP-03 · Routing rules](/plans/tp-03-routing)
- [ ] [TP-04 · Inbound TLS](/plans/tp-04-tls) — if TLS or listeners changed
- [ ] [TP-05 · Inbound AUTH](/plans/tp-05-auth) — if AUTH or credentials changed

### If the queue or delivery changed

- [ ] [TP-06 · Queue and retries](/plans/tp-06-queue)
- [ ] [TP-07 · Bounces and DSN](/plans/tp-07-dsn)

### If Central Management changed

- [ ] [TP-08 · Claim and provisioning](/plans/tp-08-provisioning)
- [ ] [TP-09 · Deploy and rollback](/plans/tp-09-deploy-rollback)

### If the bundle format or the store schema changed

- [ ] TP-08 and TP-09 **in full**
- [ ] An upgrade from the **previous released version**, not from a clean state

::: danger Rolling back past a store migration bricks a gateway
`internal/store` migrations are forward-only and the gateway refuses a database
newer than the binary. Test the upgrade *and* decide explicitly whether a
rollback is possible — recovery is replacing the data volume, which destroys the
node's identity and forces re-approval.
:::

## Upgrade rehearsal

Not optional for anything touching configuration or storage.

1. Deploy the **previous** released version and put it into a realistic state:
   mail in the queue, something quarantined, a gateway approved and configured.
2. Upgrade in the documented order — migrations first (`deploy/core/upgrade.sh`),
   then services.
3. Verify:
   - [ ] queued mail still delivers — **it was written by the old version**
   - [ ] quarantined envelopes are still listed and can still be released
   - [ ] the gateway keeps its identity and stays approved
   - [ ] its applied configuration version is unchanged
   - [ ] existing log rows are still searchable

**Step 3's first item is the one that catches spool format mistakes.** A new
envelope field that is not `omitempty` at the current version makes every
envelope already on disk fail validation — and they are then moved to `dead/`,
which parks the entire live queue.

## Recording

For each plan: **pass / fail / blocked / not run**, with the reason for anything
that is not a pass. Attach transcripts for SMTP steps.

Note explicitly what you **did not** run and why. "Not run — no change to the
queue path" is a useful record; silence is not.

## Sign-off

| | |
|---|---|
| Version | |
| Date | |
| Run by | |
| Stack | file mode / managed |
| Plans run | |
| Plans skipped, and why | |
| Failures | |
| Known issues accepted | |
