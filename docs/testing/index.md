---
layout: home
hero:
    name: Test plans
    text: Manual verification for the mailgw stack
    tagline: Preconditions, numbered steps, expected results. Written to be executed and recorded, not read.
    actions:
        - theme: brand
          text: How to use these
          link: /how-to-use
        - theme: alt
          text: Start with TP-01
          link: /plans/tp-01-smoke
---

## What is here

Ten test plans covering the things the automated suites cannot check: a real
relay, a real browser, a real network, and a person deciding whether what they
see is right.

| | Plan | Covers |
|---|---|---|
| TP-01 | [Smoke test](/plans/tp-01-smoke) | the stack is alive and mail flows |
| TP-02 | [IP allowlist](/plans/tp-02-allowlist) | the inbound gate, fail-closed behaviour |
| TP-03 | [Routing rules](/plans/tp-03-routing) | per-recipient routing, stage timing, split |
| TP-04 | [Inbound TLS](/plans/tp-04-tls) | STARTTLS, implicit TLS, renewal |
| TP-05 | [Inbound AUTH](/plans/tp-05-auth) | credentials, gating, rule fields |
| TP-06 | [Queue and retries](/plans/tp-06-queue) | deferral, backoff, quarantine, mailq, the pool cap, MX caching |
| TP-07 | [Bounces and DSN](/plans/tp-07-dsn) | notifications and RFC 3461 parameters |
| TP-08 | [Claim and provisioning](/plans/tp-08-provisioning) | first boot to first configuration |
| TP-09 | [Deploy and rollback](/plans/tp-09-deploy-rollback) | versioned configuration |
| TP-10 | [Observability](/plans/tp-10-observability) | endpoints, counters, audit trail |

Then the [release checklist](/release-checklist) for what to run before shipping.

## If a plan could be a `go test`, it should be one

These exist because they need something a unit test cannot have. A plan that
could be automated is a bug in this collection — [say so](/reporting) rather than
running it by hand for ever.
