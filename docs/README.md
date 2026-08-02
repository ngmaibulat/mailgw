# docs/

Three VitePress sites, each its own pnpm workspace member.

| Directory | Package | Port | Audience |
|---|---|---|---|
| `public/` | `mailgw-docs-public` | 5173 | customers and operators |
| `internal/` | `mailgw-docs-internal` | 5174 | engineers working on the repo |
| `testing/` | `mailgw-docs-testing` | 5175 | whoever is running a manual test pass |

```bash
pnpm install              # from the repo root
pnpm docs:public:dev
pnpm docs:internal:dev
pnpm docs:testing:dev
pnpm docs:build           # build all three
```

They run on different ports on purpose, so all three can be open at once.

## Why three sites and not one

The split is an **audience** boundary, and it is load-bearing for the first one:

**`public/`** describes behaviour an operator can observe — what the gateway
does, how to configure it, what a reply code means. It must not contain milestone
plans, audit findings, defect histories or "what was built differently" notes.
Several of those describe security defects and the reasoning behind credential
handling, and they are engineering context rather than product documentation.

**`internal/`** is the engineering site: architecture, conventions, standing
decisions and known gaps.

**`testing/`** is executable — preconditions, numbered steps, expected results,
meant to be run by a person and recorded.

## What is deliberately not here

The living engineering documents stay where they are, because other files
cross-reference them by relative path and copying them would give two versions
that drift:

- `plans/M<n>-*.md` and `plans/README.md` — the milestone plans
- `mailgw-go/TODO.md`, `webui-fastify/TODO.md` — the backlogs
- `CLAUDE.md` — the curated project overview
- `deploy/README.md` — production topology
- `logservice/docs/api.md` — the log service endpoint reference

`internal/` links to them and covers what they do not.

## Conventions

- **`public/` sources from observable behaviour.** Where a page states a reply
  code, a default or a field name, it should be checkable against
  `mailgw-go fields`, `check`, or the code. Prefer telling the reader to run the
  command over copying its output.
- **Cross-site links do not resolve** — these are three separate VitePress
  builds. Refer to a sibling site by name (`docs/testing`) rather than with a
  relative link, or VitePress fails the build on a dead link.
- Local search is on for all three; no external search service is configured.

## Deployment

None yet, deliberately. All three build to `.vitepress/dist` with `base: '/'`.
Adding a host is a one-line `base` change per site plus whatever publishes the
output.
