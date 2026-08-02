# How to use these

## The shape of a plan

Every plan has the same four parts:

**Purpose** — what it establishes, and what it does not.

**Preconditions** — the state the system must be in before step 1. If you cannot
satisfy them, stop; a plan run from the wrong starting state proves nothing.

**Steps** — numbered, each with an expected result. Do them in order.

**Cleanup** — how to get back to the starting state, so the next plan is not run
against your leftovers.

## Recording a run

Record, per step: **pass**, **fail**, or **blocked**, plus the actual output when
it is not a pass. A result of "worked" without the reply code is not a result —
the whole point of an expected result written down is that two people running the
plan agree on what they saw.

Keep the transcript. For SMTP steps that means the whole session, both
directions; `swaks` prints it by default.

## When a step fails

**Stop the plan** unless a later step is genuinely independent. A failure usually
invalidates everything after it, and pushing on produces a report nobody can act
on.

Then see [Reporting a failure](/reporting).

## Conventions in these plans

Commands are written for the [test environment](/environment). Where a plan needs
a value from your own setup it is written in angle brackets: `<gateway-host>`,
`<uuid>`.

Replies are quoted as the code and the enhanced status:

> `550 5.7.1 …`

When a plan says a reply must be **permanent** or **temporary**, the first digit
is the thing being tested — `5xx` versus `4xx`. That distinction decides whether
the sender retries for four days, and several of these plans exist only to check
it.

::: tip Run TP-01 first, always
Every other plan assumes the stack is alive and mail flows. Ten minutes of TP-01
saves an afternoon of debugging a plan that was never going to pass.
:::

## What these plans do not cover

- **Load and performance.** Nothing here measures throughput or latency.
- **Anything the Go suite already asserts.** The rule engine's matching
  semantics, the DSN report format, the spool's crash safety — those are unit and
  contract tests, and duplicating them by hand adds nothing.
- **Message authentication.** SPF, DKIM and DMARC are not implemented.
