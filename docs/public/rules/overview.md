# Routing rules

`routing.yaml` holds two ordered lists that share one matcher:

- **`policy`** — whether to accept this message or recipient
- **`routes`** — where to send it

```yaml
version: 1

policy:
    - name: block-executables
      priority: 100
      match: {field: attachment.filename, op: glob, value: "*.exe"}
      then:
          - {action: reject, code: 550, message: "5.7.1 executable attachments are not accepted"}

routes:
    - name: internal
      priority: 100
      match: {field: rcpt.domain, op: eq, value: "internal.example"}
      then:
          - {action: relay, relay: Exchange}

    - name: everything-else
      priority: 1000
      match: {always: true}
      then:
          - {action: relay, relay: Outbound}

default_action:
    action: tempfail
    code: 451
    message: "4.3.0 No route found"
```

## How a rule is chosen

Rules sort by **`priority` ascending, then file order**, and the **first match
wins**. Lower priority numbers are considered first — think of it as "rank 1 is
looked at first", not "higher is stronger".

Policy and routes are evaluated independently. A message can be accepted by
policy and still find no route, in which case `default_action` applies.

## Stage inference

You do not declare when a rule runs. Its stage is the **latest stage of any field
it mentions**, so a rule that reads only `rcpt.domain` fires at `RCPT TO`, and
adding one condition on `msg.size` moves the whole rule to the end of `DATA`.

The router walks rules in priority order and **stops at the first one that needs
a later stage**, reporting "undecided". That is what makes an early decision
trustworthy: if the walk got to an answer without being blocked, that answer is
provably the one `DATA` would have reached.

`default_action` only applies at `DATA`, so a recipient nothing routes is not
refused until the message is complete.

You can pin a rule later than inference would put it with an explicit `stage:`,
but you cannot pin it earlier — the facts would not exist.

## It is not a programming language

By design. No loops, no user code, no arithmetic. Predicates are a typed tree,
regular expressions are Go's RE2 (no backtracking, so no catastrophic-blowup
failure mode), and every field name is checked against a registry when the
configuration loads.

That last point is worth dwelling on: **a typo is a load-time error, not a rule
that silently never fires.** `rcpt.doman` will not start the gateway, and `check`
will suggest what you meant.

## Seeing what a rule does

```bash
mailgw-go explain --rcpt bob@partner.com --from alice@example.com
```

It prints every policy and route rule, whether each matched, at which stage, and
what the outcome was. `-eml <file>` populates the data-stage fields from a real
message; `-auth-user` models an authenticated session; `-tls` an encrypted one.

## The older format

`routing.json` in the four-field Haraka format still works and is transpiled into
the same compiled ruleset — an existing deployment is untouched. To move:

```bash
mailgw-go convert-routing config/routing.json > config/routing.yaml
```

Two long-standing bugs are fixed in the translation: an address with no `@` now
yields an empty domain rather than the whole string, and an unknown relay group
is a hard configuration error rather than silently resolving against a built-in
object property.

## Next

- [Matching](/rules/matching) — predicates, operators, globs
- [Actions](/rules/actions) — what a rule can do
- [Field reference](/rules/fields) — every field you can match on
