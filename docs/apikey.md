# API-key authentication

The logservice protects its endpoints with an optional API key. When enabled,
the Haraka plugins must send the same key or their requests are rejected.

## How it works

- **logservice** (`logservice-bun`) reads `API_KEY` from the environment
  (`Bun.env.API_KEY`). If it is set, every request must carry a matching
  `X-API-Key` header or it gets `401 Unauthorized`. If it is unset/empty,
  auth is disabled and all requests are accepted.
- **Haraka plugins** read `API_KEY` from the environment
  (`process.env.API_KEY`) and attach it as the `X-API-Key` header on every
  call to the logservice — both the logging POSTs (`functions.httplog`) and the
  attachment check (`AttachChecker` → `POST /filter/md5`). When `API_KEY` is
  unset, no header is sent.

Both sides must use the **same value**. Setting it on only one side breaks
things: key only on the logservice → plugins get 401; key only on Haraka → the
server ignores a header it doesn't require (no protection).

## Enabling it

`docker-compose.yaml` wires `API_KEY` into both the `mailgw` and `logservice`
services from a single host value, so they stay in sync:

```yaml
environment:
    API_KEY: ${API_KEY:-}   # empty default = auth disabled
```

Provide the value via a `.env` file next to `docker-compose.yaml`:

```
API_KEY=choose-a-long-random-secret
```

or export it before bringing the stack up:

```bash
export API_KEY="choose-a-long-random-secret"
docker compose up
```

Generate a strong value with e.g. `openssl rand -hex 32`.

## Running without auth (dev)

Leave `API_KEY` unset (or empty). The logservice accepts all requests and the
plugins send no key — the default for local development.
