- Two-stage build — deps stage installs only production dependencies (no @types/bun, no test tooling), then the final image copies just node_modules, src/, migrations/, and migrate.ts. Tests and docs never enter the image.
- oven/bun:1-alpine — official Bun image on Alpine, keeps the image small.
- CMD is the server only — bun src/index.ts with no --migrate flag. The db-migrator service in docker-compose.yaml overrides the command to add --migrate, so migration and serving remain separate as we designed.

To build and push:

```bash
docker build -t ngmaibulat/mailgw-logservice-bun:latest .
docker push ngmaibulat/mailgw-logservice-bun:latest
```
