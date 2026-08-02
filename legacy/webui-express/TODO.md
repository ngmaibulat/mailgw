# webui-express TODO

Consolidated roadmap for the mailgw web UI (Express + Sequelize, with an
experimental Deno target). This merges and supersedes the scattered notes in
`todo.md`, `todo-01.md`, `todo-02.md`, `todo-03.md` (kept for now), with status
derived from reading the current code. `.github/doc.md` is just a bookmark URL,
not a task.

**Legend:** `[x]` done · `[~]` partial / stubbed · `[ ]` not started

---

## Snapshot — what exists today

- HTTPS/HTTP2 server via `spdy` (`src/index.mjs`); requires `./certs/server.{key,crt}`.
- Session login (`/login`, bcrypt vs `User.hash`), in-memory session store, `checkSession` guard.
- Log viewer pages (pug + ejs): connection, delivery, mails, lookups.
- Ingestion + query API: `/api/{connection,delivery,queue,hashlookups}`.
- Attachment MD5 filter endpoint: `/filter/md5`.
- Relay & relay-group CRUD under `/config` (`CtrlRelay`, `CtrlRelayGroup`).
- Experimental Deno target (`adapter.deno.js`, `deno-index.mjs`).
- CLI user tools: `create_user.mjs`, `check_user.mjs`.

## User management  _(was todo-01.md)_

- [~] User CRUD — CLI only (`create_user.mjs`, `check_user.mjs`); no web UI
- [ ] Profile page — route exists but renders `util/notimpl`
- [ ] Roles: `admin`, `logview`, custom (view certain logs with filter) — `User` model has only `email` + `hash`
- [ ] "View my logs" (mail sent to the logged-in user)
- [ ] Register / approve mechanism
- [ ] Email verification
- [ ] Reset password via email
- [ ] AD / LDAP authentication

## Routing & relay management  _(was todo.md "Manage Routing/Relays")_

- [x] Relay CRUD (`/config/relay/*`)
- [x] Relay-group CRUD (`/config/relaygrp/*`)
- [ ] Routing-rule management — `/config/routing` renders `util/notimpl`
- [ ] Export JSON configs (generate mailgw `relays.json` / `routing.json`)

## Logging & filtering features  _(was todo.md "Features")_

- [x] Log connections (`/api/connection` + viewer)
- [x] Log deliveries (`/api/delivery` + viewer)
- [x] Log queue/transactions (`/api/queue` + viewer)
- [x] Attachment filtering service (`/filter/md5`, `hashListLookup` + lookups viewer)
- [ ] Quarantine

## Mail security  _(was todo.md "Features")_

- [ ] DKIM signing / checking
- [ ] DMARC checking / reporting

## Deployment  _(was todo-02.md)_

- [ ] Dockerfile (none present)
- [~] CI — `.github/workflows/publish.yml` publishes an S3 tarball; no container image build
- [ ] Build container via GitHub Actions
- [ ] Kubernetes manifests
- [ ] Helm chart

## Multi-runtime / Deno  _(was todo-03.md)_

- [~] Deno version — `adapter.deno.js` + `deno-index.mjs` exist (untested in CI)
- [ ] Build as a single executable
- [ ] Run as Docker (Deno)

## Provisioning (advanced, aspirational)  _(was todo.md "Advanced")_

- [ ] Provision mail gateways via SSH
- [ ] Provision via cloud-provider API
- [ ] Provision via virtualization API
- [ ] Provision via Kubernetes API
- [ ] Push env to existing mail gateways

## Platform / observability  _(was todo.md "Advanced")_

- [ ] RBAC (depends on Roles, above)
- [ ] SIEM integration
- [ ] ~~Log Service in Golang~~ — likely **obsolete**: the monorepo now ships a Bun `logservice/`; decide whether to keep this idea

## Testing  _(was todo-03.md "Test SMTP Relay")_

- [~] SMTP relay tests — only trivial HTTP stubs here (`client/send.js`, `client/http.rest`); real SMTP e2e now lives in the monorepo `tests/smtp/`. Outstanding: send simple email; customized (from/to/cc/subject/body); with attachment; custom SMTP sender/rcpt; log the test runs
- [ ] App unit/integration tests — `package.json` `test` runs `node --test` but there are no test files

## Tech debt & open questions (from code review)

- [ ] **Overlap with `logservice/`.** This app and the Bun `logservice/` define the same models (Connection/Delivery/Transaction/hashlookups) and expose the same `/api/*` + `/filter/md5` endpoints. Decide the split (e.g. webui = UI only, logservice = ingestion API) to avoid two diverging implementations.
- [ ] **Unvalidated, fire-and-forget writes.** `/api/{delivery,connection,queue}` pass `req.body` straight into Sequelize `create()` without `await`, validation, or error handling (cf. logservice's Zod schema).
- [ ] **TLS certs required to boot.** `src/index.mjs` reads `./certs/server.{key,crt}` unconditionally — document/provision, or allow plain-HTTP for local dev.
- [ ] **In-memory sessions** (`src/globals.mjs`) — lost on restart, not multi-instance safe.
- [ ] **Two template engines** (pug + ejs) maintained in parallel — pick one.
