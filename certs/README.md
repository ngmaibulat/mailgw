# certs

Generates self-signed TLS certificates from a JSON config. Bun project (own
`bun.lock`), standalone like `logservice/` and `tests/` — not a pnpm workspace
member. Uses `node-forge` (pure JS), so no `openssl` binary is required.

## Usage

```bash
cd certs
bun install
bun run generate            # reads ./certs.config.json
bun run generate other.json # explicit config path
```

Or from the repo root: `pnpm certs` (alias for `bun certs/src/generate.ts`).

## Config (`certs.config.json`)

`defaults` are merged into each entry in `certs`:

```json
{
  "defaults": { "days": 825, "keySize": 2048, "organization": "NGM Mailgw",
                "keyFile": "server.key", "certFile": "server.crt" },
  "certs": [
    { "name": "webui", "commonName": "localhost",
      "altNames": ["localhost", "dev-webui", "127.0.0.1", "::1"],
      "out": "generated/webui" }
  ]
}
```

| field | meaning |
|---|---|
| `name` | label for logs |
| `commonName` | subject/issuer CN |
| `altNames` | Subject Alternative Names; DNS names and IPs are detected automatically |
| `out` | output dir (relative to this project, or absolute) |
| `keyFile` / `certFile` | filenames within `out` (default `server.key` / `server.crt`) |
| `days` / `keySize` / `organization` | validity, RSA size, org |

## Output

Files are written under each entry's `out` directory (default `generated/...`)
and are **gitignored**. The `webui` entry produces
`generated/webui/server.key` + `server.crt`, which `docker-compose.yaml` mounts
into the webui container at `/app/certs`.

> These are self-signed dev certs — browsers will warn. Replace with real certs
> in production.
