### Basic

- initial config script
- generate json configs with secrets/addrs/ports from .env

### Scaffolder:

- check env vars
- check if smtp destinations are reachable
- check if git, node, pnpm, pm2 tools are installed
- generate bash code at the end
- run via eval `npx toolname`
- bash code should:
- clone repo
- rm -rf .git
- pnpm install
- show help to run haraka
- show help to update smtp routing

### Docker

- docker build
- docker hub

### Kuber

- kubernetes
- tls

### Logging

- logging plugin
- logger service
