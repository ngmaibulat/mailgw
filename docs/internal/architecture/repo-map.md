# Repository map

A mixed **pnpm + Bun + Go** monorepo. That mix is deliberate and the boundaries
matter — getting them wrong is the most common way to waste an afternoon here.

```
mailgw/
├── mailgw-go/          Go module      the gateway
├── logservice/         Bun            audit API over MariaDB
├── webui-fastify/      pnpm  ★        admin console + Central Management
├── tests/              Bun            cross-cutting end-to-end tests
├── certs/              Bun            self-signed cert generator
├── docs/               pnpm  ★        these three documentation sites
│   ├── public/
│   ├── internal/
│   └── testing/
├── deploy/             shell          production compose, split by role
├── plans/              markdown       milestone plans
└── legacy/             frozen         Haraka era
    ├── mailgw/             pnpm, NOT a member
    ├── webui-express/      pnpm, NOT a member
    ├── deploy/
    └── docs/

★ = pnpm workspace member
```

## Package-manager boundaries

**Only `webui-fastify` and `docs/*` are pnpm workspace members.**

`logservice/`, `tests/` and `certs/` are Bun projects with their own `bun.lock`,
deliberately excluded so pnpm and Bun do not fight over `node_modules`.
`mailgw-go/` is a Go module. `legacy/mailgw` and `legacy/webui-express` are pnpm
projects that are **not** members, so a root `pnpm install` ignores them —
install one standalone with `cd legacy/mailgw && pnpm install`.

The practical consequences:

- Do not run pnpm scripts that assume a Node toolchain against a Bun project.
- `pnpm --filter @aibulat/mailgw` does **not** resolve. That package is outside
  the workspace.
- Root scripts sometimes call Bun directly — `pnpm certs` runs
  `bun certs/src/generate.ts`.

## What the root scripts mean

`pnpm start`, `dev`, `check` and `test` all mean **mailgw-go**. It is the gateway
now. Haraka keeps explicit `legacy:*` names.

```bash
pnpm start          # run mailgw-go against mailgw-go/config
pnpm dev            # check the config first, then run
pnpm check          # validate, non-zero on error
pnpm test           # go test ./... plus the logservice Bun suite
pnpm test:e2e       # end-to-end, needs a running stack
pnpm docs:public:dev
```

## Where to look first

| Question | File |
|---|---|
| What does this project do? | `CLAUDE.md` |
| What are the exact commands? | `CLAUDE.md`, `AGENTS.md` |
| Which packages are pnpm? | `pnpm-workspace.yaml` |
| What is planned or done? | `plans/README.md` |
| What is broken or missing? | `mailgw-go/TODO.md`, `webui-fastify/TODO.md` |
| How is it deployed? | `deploy/README.md` |

## Container images

Each Node/Bun service has `container-build.sh` (local) and `container-push.sh`
(build and push), tagging `ngmaibulat/<name>:v<ver>` and `:latest`.

The console and the two legacy images build from the **repository-root context**
via `-f <path>/Dockerfile`; `logservice` and `mailgw-go` build from their own
directories. The four legacy scripts `cd` two levels up to reach that context.

```bash
pnpm run docker:push       # the modern stack: mailgw-go + logservice + webui-fastify
pnpm build:containers      # that, plus the legacy Haraka image
```

::: warning mailgw-go's build does not bump its version
Unlike the Node container scripts, `mailgw-go/container-build.sh` deliberately
does not mutate the tree. Use `./bump.sh` explicitly. `docker:push` calls it for
you.
:::
