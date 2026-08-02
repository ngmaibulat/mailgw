# M2 — The routing DSL

**Status:** done  ·  **Package:** `mailgw-go`  ·  **Depends on:** M1  ·  **Blocks:** M8

> Migrated verbatim from `mailgw-go/TODO.md` on 2026-07-29. The task list and
> the two "why" sections are unchanged; the overview and the file references
> below were added at migration time from `CLAUDE.md`.

## Goal

Replace Haraka's four-field routing table with a declarative, typed rule engine —
and make a rule that will never fire a **load-time error** rather than a silent
no-op. This is the reason the rewrite exists.

`routing.yaml` holds two ordered lists sharing one matcher: `policy` (accept or
not) and `routes` (where to send). Predicates are a typed AST — `all` / `any` /
`not` / `every` / `always` over `field` / `op` / `value` leaves. Rules sort by
`(priority asc, file order)`, first match wins.

**Stage inference is what fixes RCPT timing.** A rule's stage is the latest
stage of any field it mentions, so a rule reading only `rcpt.*` fires at RCPT and
its rejection reaches the client on its own `RCPT TO` line. `Route` walks rules
in priority order and stops at the first one needing a later stage, reporting
"undecided" — so an early decision is provably the one DATA would reach, and can
be cached. The `default_action` only applies at DATA, preserving Haraka's
`hook_get_mx` timing for "No route found".

`routing.json` still works and **transpiles** into the same compiled ruleset
(`transpile.go`), so an existing deployment is untouched. Equivalence is
asserted, not assumed: `transpile_test.go` runs the legacy matcher and the DSL
over the same envelope matrix. Two legacy bugs are fixed in passing — `Domain()`
returns `""` for an address with no `@` (`Route.js:16` returned the whole
string), and an unknown relay group is a hard config error rather than a
prototype-chain hit (`RoutingTable.js:29` used `name in relays`, so
`relay: "toString"` resolved to a Function).

## Delivered

- [x] Field schema registry with per-field stage and kind
      (`conn.*`, `helo.*`, `auth.*`, `mail.*`, `rcpt.*`, `msg.*`, `header.*`,
      `attachment.*`, `tag.*`) — `internal/ruleset/schema.go`
- [x] Predicate AST + compiler + validator (`all`/`any`/`not`/`every`/`always`;
      operators `eq ne contains prefix suffix glob regex in in_cidr lt le gt ge
      exists empty`)
- [x] Stage inference, so a rule evaluates as early as its fields allow — a
      per-recipient reject fires at RCPT, not at DATA
- [x] Actions beyond `relay`: `reject`, `tempfail`, `discard`, `quarantine`,
      `add_header`, `tag`, `accept`
- [x] `explain` subcommand, plus `fields` for the registry
- [x] `convert-routing routing.json > routing.yaml`
- [x] Hot reload of the full ruleset on `SIGHUP`, atomic swap, running
      configuration retained on validation failure
- [x] Rules stay declarative and non-Turing-complete: no loops, no user code,
      RE2 only. Hold this line.

### Two glob dialects, deliberately

`*` stops at a dot for domain-shaped fields (`*.partner.com` matches
`mx.partner.com`, not `partner.com`) and crosses dots elsewhere (`*.exe` matches
`report.q3.exe`). A leaf over a list field is **existential**, so
`not: {attachment.filename glob "*.exe"}` reads as "no attachment is a .exe";
`every` is the universal form. Implemented in `glob.go` rather than pulling in
`gobwas/glob`.

## Deliberately not done, and why

- **fsnotify auto-reload.** A config file being saved is observed mid-write, so
  it would log a spurious error on every save even with keep-on-error. `SIGHUP`
  is the signal that actually means "I finished editing".
- **Live relay reload.** Rules reload; `relays.json` does not, because swapping
  the relay table under in-flight deliveries needs runner support. A reload that
  would require it is refused with a message rather than half-applied.
- **`conn.remote_host` (rDNS).** Left out of the registry entirely: adding it
  without the lookup would give a field that silently never matches, which is
  precisely what the registry exists to prevent.

## Follow-ups the DSL created

- [ ] A recipient refused by a *data-stage* rule is dropped with a WARN, because
      the SMTP reply is already spent. It should get a DSN — folded into M7.
- [ ] `msg.has_attachment`, `msg.mime_part_count` and `attachment.*` are in the
      registry but never populated until the MIME walk lands in M8. `check` and
      startup warn when a rule uses one; remove the warnings with the feature.
- [ ] Route decisions are not recorded in the audit events, so the log tables
      cannot answer "which rule sent this message here?". Needs a logservice
      column — pairs naturally with the `gateway` column in M6.

## Defect found after the fact

**[M9.1](./M9-correctness-and-durability-fixes.md) is a bug in this
milestone's session integration**, not in the rule engine itself: a
recipient-scoped *data-stage* policy rule is never evaluated when that
recipient's route already resolved at RCPT. The stage-caching property described
above is sound for **routing**; it was wrongly applied to **policy** as well.
Confirmed against a running server. Fix it before M8.
