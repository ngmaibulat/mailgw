### 2026-06-14

- db migrator script fixes
- Debugged the SMTP 550/554 chain → config + IP allowlist + container networking fixes
- logservice dt datetime fix, Transaction insert wired up, duplicate Connection write removed
- Monorepo restructure into mailgw/, workspace split (logservice on Bun), root scripts
- deploy/ modernized for the 2-edge + 1-core topology (no pm2), secrets flagged
- Docker modern-syslog build fix, Haraka upgraded to 3.3.1 (with the minimumReleaseAgeExclude placement gotcha)
- Bun-native SMTP test client + 19 tests, verified against the live stack

A couple of things still open whenever you want them: the committed tls_key.pem / relays.json secrets under deploy/mailgw/settings/, and trimming the Haraka minimumReleaseAgeExclude once 3.3.1 ages past the 7-day window (~Jun 19).

### 2026-06-07

- removed dep: "mimelib": "^0.3.1"
- switched to PNPM@11 via corepack
- added pnpm-workspaces.yaml to allow build of modern-syslog (Haraka dependency)
- switched to NodeJS 26 from 20
- added: `sudo pacman -S docker-buildx`
- run: `docker buildx version`: `github.com/docker/buildx 0.34.1 e0b0e77d18d3379bc1e0d55f3b37de288d36fe47`
- build and push via buildx
- use pnpm to get version automatically: `pnpm pkg get version`
- use `pnpm version patch` to bump version
- use: `pnpm config set minimum-release-age 1440`
- set: `blockExoticSubdeps: true`
- added trivy scan
- `connection.ini` is now mandatory. Use config examples: `https://github.com/haraka/Haraka/blob/master/config/connection.ini`

### Todo

- Test on Ubuntu 26.04
- Start using Claude Code

### Document

- `pnpm clean --lockfile`
- https://pnpm.io/supply-chain-security
- https://github.com/haraka/Haraka/blob/master/config/connection.ini
