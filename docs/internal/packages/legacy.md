# legacy/

The Haraka implementation this project replaced, its Express console, and the Bun
log service. All frozen.

**Prefer changing the modern stack. Touch `legacy/` only to keep it building.**

```
legacy/
├── mailgw/            the Haraka SMTP server and its plugins
├── webui-express/     the Express console
├── logservice/        the Bun log service, superseded by logservice-go (M21)
├── deploy/            its production compose
├── docs/              its notes
└── archive/
```

None of these is a pnpm workspace member, so a root `pnpm install` ignores them.
`legacy/logservice/` is a standalone **Bun** package with its own `bun.lock` and
is still wired into `pnpm test`. Install one standalone:

```bash
cd legacy/mailgw && pnpm install
pnpm legacy:start        # from the repository root
```

## Why it is still here

Two reasons, both practical:

1. **Parity is checkable.** The Haraka service is commented out in
   `docker-compose.yaml` rather than deleted — uncomment it, move the gateway off
   host port 25, and both can run side by side against the same log service.
   Their identity spaces are disjoint, and the gateway stamps
   `X-NGM-Gateway: go` on relayed mail.
2. **The plugins are the specification.** Several behaviours in the gateway exist
   because a plugin did something, and several were fixed because a plugin did it
   wrong. The comments in `mailgw-go` cite plugin filenames and line numbers.

## What the rewrite fixed

Worth knowing, because the tests still assert these:

**Routing was four fields and one operator** — case-insensitive equality, ANDed,
so `ngm.dev` did not match `mail.ngm.dev`. And the whole message was routed by
`rcpt_to[0]`, so a message to two domains followed the first recipient's route.

**Delivery events were being lost silently.** The plugin sent `rcpt_accepted` as
a comma-joined list where the log service validated a single address, so every
multi-recipient delivery `400`ed — invisibly, because the POST was
fire-and-forget with no status check.

**Two attachment-scanning bypasses.** An `inline` part naming a file was not
treated as an attachment, and a malformed MIME structure returned "allow".

## Plugin quirks, if you must read them

The plugin set grew organically and is **not** uniform. Do not assume symmetry:

- **`npConnection` does not post to the log service** despite the name — it
  writes a local file and has a dead blacklist placeholder that always returns
  false. The plugin that posts connection events is `npData`, at `hook_data`.
- **`npFilterAttach` hardcodes `http://localhost:3000`** while the others read
  `logging.json`, so its calls break wherever the log service is not co-located.
- **`npFilterAttach` also posts connection and transaction events**, overlapping
  `npData` and `npQueue` — a duplicate-row risk.
- **All posts are fire-and-forget.** Failures are logged locally, never retried,
  never surfaced to the SMTP transaction.

`legacy/webui-express` carries the baggage the Fastify rewrite shed: dual pug and
ejs engines, an abandoned Deno adapter, a duplicated full model set, and
overlapping ingest endpoints.
