# Conventions

## Comments explain why, not what

This codebase is unusually heavily commented, and the comments carry the
reasoning that would otherwise be lost. The convention is:

- **State the trap.** "`Deliver`'s defers run LIFO, so `Pool.Put` runs before
  `defer guard.release()` — once the guard reaches a pooled conn that ordering is
  load-bearing."
- **Name what was rejected and why.** "A reference-counting file was considered
  and rejected: a lost decrement leaks a body, but a lost increment **deletes a
  live one**."
- **Cite the source.** Plugin filenames and line numbers, RFC sections, upstream
  library line numbers.

A comment that restates the code is noise. A comment that says why the obvious
alternative is wrong is the most valuable line in the file.

::: danger Do not let a comment overstate what a fix does
If a comment claims a line prevents a failure, that claim should be testable. One
comment in this repository asserted that clearing two fields prevented a leak
between transactions; the library in fact overwrites them unconditionally, so the
clearing is hygiene rather than the mechanism. The comment was corrected to say
so. An overstated comment is worse than none — it stops the next reader looking.
:::

## Milestones

Work is organised into milestones, one file per milestone in `plans/`, status on
line 3, indexed by `plans/README.md`.

**Milestone numbers are identity: never reused, never renumbered.** A new
milestone takes the next free number even when it should be worked first —
running order lives in the index, not in the number.

Every finished milestone gets a **"What was built differently"** section
recording where the plan was wrong. This is not ceremony: several milestones
found their own plan contained a dead end, and the record is what stops the next
one repeating it.

## Go

- `gofmt` clean, `go vet` clean, `go test -race` green. Not negotiable.
- Errors wrap with `%w` and name the file or uuid they concern.
- Exported identifiers carry doc comments; unexported ones carry them when the
  reasoning is not local.
- Structs compared with `!=` in `restartRequired` **must stay comparable** — no
  slices or maps. `Limits` says so in a comment for that reason.

## TypeScript

Biome, configured in `biome.json`: recommended rules, 4-space indent, double
quotes, 80 columns, semicolons.

Import sorting is **off** on purpose, so the node / third-party / local grouping
survives. Unused Fastify handler parameters take a `_` prefix.

```bash
pnpm --filter mailgw-webui-fastify check:fix
```

## SQL migrations

Numbered, forward-only, essay-commented. See
[logservice](/packages/logservice#migrations).

## Configuration keys

- **An explicit `0` that would disable a bound is an error, not a way to switch
  it off.** `max.connections: 0`, `inactivity_timeout: 0` and `attach.timeout: 0`
  are all rejected. Defaults are unmarshalled over, so a key present with a zero
  value is indistinguishable from someone meaning it — and a bundle is not a file
  an operator proofreads.
- **Optional fields are omitted when empty**, so an unchanged configuration keeps
  hashing identically.
- **A new feature ships off** when turning it on changes what every message costs
  or what every relay sees.

## Counters

Adding one is exactly two edits — a field on `Metrics`, and an entry in the
`counters` table — plus a line in the golden key list. Tests enforce that every
field appears in the table exactly once and that names end in `_total`.

**State the unit in the HELP string**, and any subset or superset relationship.
See [Standing decisions](/architecture/decisions).

## Documentation

When behaviour changes on the wire or in a configuration file:

| Change | Update |
|---|---|
| Anything an operator sees | `CLAUDE.md` and the public docs site |
| Milestone finished | its `plans/` file, `plans/README.md`, the package TODO |
| A gap you are leaving | the relevant `TODO.md`, with the reason |
| A decision you are making | [Standing decisions](/architecture/decisions) |
