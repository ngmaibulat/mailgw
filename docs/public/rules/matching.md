# Matching

A `match` is a tree. Every node is either a **combinator** or a **leaf**, and a
node with two branch keys is a configuration error rather than a silent
precedence guess.

## Combinators

```yaml
match:
    all:                                   # every child must match
        - {field: rcpt.domain, op: eq, value: "partner.com"}
        - any:                             # at least one child must match
              - {field: mail.from_domain, op: eq, value: "example.com"}
              - {field: auth.authenticated, op: eq, value: true}
        - not:                             # invert
              field: msg.size
              op: gt
              value: 10485760
```

| Key | Meaning |
|---|---|
| `all` | every child matches |
| `any` | at least one child matches |
| `not` | the child does not match |
| `every` | for a **list** field: every element matches (see below) |
| `always: true` | the constant predicate |

## Leaves

```yaml
{field: <name>, op: <operator>, value: <value>}
{field: <name>, op: in, values: [<a>, <b>]}
{field: <name>, op: eq, value: "X", ci: false}
```

`ci` overrides the field's default case sensitivity. Most string fields — the
address and domain ones especially — are case-insensitive already.

## Operators

| Operator | Types | Meaning |
|---|---|---|
| `eq` / `ne` | any | equal / not equal |
| `contains` | string | substring |
| `prefix` / `suffix` | string | starts / ends with |
| `glob` | string | wildcard match, see below |
| `regex` | string | RE2 regular expression |
| `in` | any | equal to any of `values` |
| `in_cidr` | ip | inside a CIDR prefix |
| `lt` / `le` / `gt` / `ge` | int | numeric comparison |
| `exists` | any | present and non-empty |
| `empty` | any | absent or empty |

An operator that does not fit the field's type is a **compile error**:
`msg.size gt "big"` fails to load rather than surprising you at three in the
morning.

## Two glob dialects

`*` behaves differently depending on what kind of field it is applied to, and
this is deliberate:

**Domain-shaped fields** (`rcpt.domain`, `mail.from_domain`, `helo.name`) —
`*` stops at a dot:

```yaml
{field: rcpt.domain, op: glob, value: "*.partner.com"}
# matches  mx.partner.com
# does NOT match  partner.com          (no label to the left)
# does NOT match  a.b.partner.com      (* is one label)
```

**Everything else** — `*` crosses dots:

```yaml
{field: attachment.filename, op: glob, value: "*.exe"}
# matches  report.exe  and  report.q3.exe
```

The reason: a domain glob that crossed dots would make `*.partner.com` match
`evil-partner.com.attacker.test`, which is not what anybody writing that pattern
means.

## List fields are existential

Several fields are lists — every `attachment.*`, `header.<name>`,
`txn.rcpt_domains`. A plain leaf over one asks **"does any element match?"**

```yaml
{field: attachment.filename, op: glob, value: "*.exe"}
# true when AT LEAST ONE attachment is a .exe
```

Which makes `not` read naturally:

```yaml
not: {field: attachment.filename, op: glob, value: "*.exe"}
# true when NO attachment is a .exe
```

For the universal form — *every* element must match — use `every`:

```yaml
every: {field: attachment.content_type, op: prefix, value: "image/"}
# true when every attachment is an image
```

## Dynamic fields

Three prefixes take a name after the dot:

```yaml
{field: "header.subject", op: contains, value: "[URGENT]"}
{field: "header_count.received", op: gt, value: 20}
{field: "tag.scanned", op: eq, value: "yes"}
```

`header.<name>` is a list — a repeated header yields several values.
`header_count.<name>` is an int. `tag.<key>` reads a tag an earlier rule set with
the [`tag` action](/rules/actions#tag).

Everything else is enumerated: see the [field reference](/rules/fields).
