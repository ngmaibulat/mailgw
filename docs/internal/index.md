---
layout: home
hero:
    name: mailgw internals
    text: How this repository is put together
    tagline: Architecture, conventions, and the reasoning behind the decisions that are expensive to reverse.
    actions:
        - theme: brand
          text: Architecture overview
          link: /architecture/overview
        - theme: alt
          text: Start working on it
          link: /dev/getting-started
---

## What this site is for

The **public documentation site** (`docs/public`) describes what mailgw does. This site
describes **how the repository works and why it is shaped the way it is** — the
things you need before changing it, and the things that are cheap to get wrong.

## What lives elsewhere, deliberately

Some engineering documents are not reproduced here, because they are living files
that other files cross-reference by path:

| Document | Where | What it is |
|---|---|---|
| Milestone plans | `plans/M<n>-*.md` | one file per milestone, status on line 3, indexed by `plans/README.md` |
| Gateway backlog | `mailgw-go/TODO.md` | loose items, known gaps, standing decisions |
| Console backlog | `webui-fastify/TODO.md` | the same, for the admin console |
| Project overview | `CLAUDE.md` | the curated summary, kept current with the code |
| Deployment | `deploy/README.md` | production topology and procedure |
| Log service API | `logservice/docs/api.md` | endpoint reference |

Copying them here would give two versions that drift. Read them in the repository;
this site covers what they do not — the shape of the whole thing.
