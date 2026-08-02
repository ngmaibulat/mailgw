# Reporting a failure

## Before you report

**Confirm it is reproducible.** Run the failing step again from the plan's stated
preconditions. A failure that only happens once against a dirty environment is
usually the environment.

**Confirm the preconditions were met.** Most surprising failures are a plan run
from the wrong starting state — leftover mail in the queue, a configuration a
previous plan changed, a relay still stopped.

## What to include

**The plan and step.** "TP-05 step 6" is enough to locate it.

**What you expected and what you got**, both quoted. For SMTP, the whole session
in both directions — `swaks` prints it by default. Not "AUTH failed" but:

```
 -> AUTH PLAIN AGFwcEBleGFtcGxlLmNvbQBjb3JyZWN0...
 <- 454 4.7.0 Authentication credentials invalid
```

The difference between `454` and `535` there is the entire bug.

**The gateway's log** for the same period:

```bash
docker compose logs --since 5m mailgw-go
```

**The configuration.** In managed mode:

```bash
docker compose exec mailgw-go /mailgw-go config show -data /var/lib/mailgw-go
```

Secrets are redacted, so this is safe to attach. In file mode, attach the
configuration directory minus any real credentials.

**Version and mode.** `mailgw-go -version`, and whether the gateway is file-mode
or managed.

## What to leave out

**Do not attach real credentials.** `config show` redacts relay passwords, the
log service key, the metrics token and credential hashes — use it rather than
copying files by hand. If you must attach a file, check it first.

**Do not attach real customer mail.** Reproduce with a synthetic message. If the
failure genuinely depends on a real one, say so and describe its shape rather
than attaching it.

## Severity

Judge by what an operator would experience, not by how hard the step was:

| | |
|---|---|
| **Critical** | mail is lost, or accepted and silently not delivered; the inbound gate admits someone it should not |
| **High** | mail is rejected that should be accepted; a bounce is not sent when one is due; a deploy takes a gateway down |
| **Medium** | a counter or log row is wrong; a warning is missing; a CLI command misreports |
| **Low** | cosmetic, or a documentation mismatch |

::: tip Silent wrongness is worse than a loud failure
A message accepted and quietly dropped is worse than one refused with a clear
code, even though the second looks more alarming. The sender of the refused
message knows; the sender of the dropped one does not.

Score "accepted and vanished" as **critical**, always.
:::

## When a plan is wrong

Sometimes the failure is the plan. If the expected result does not match what the
system is *documented* to do, or the step is impossible to run as written, that
is a defect in this collection — report it the same way, against the plan.

Same if a plan could be an automated test. Anything here that could be a
`go test` should be one; carrying it by hand for ever is a cost with no return.
