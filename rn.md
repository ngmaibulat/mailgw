### 2026-06-08



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
