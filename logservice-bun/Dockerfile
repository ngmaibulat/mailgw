FROM oven/bun:1-alpine AS deps

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production

# ---- final image ----
FROM oven/bun:1-alpine

WORKDIR /app

COPY --from=deps /app/node_modules ./node_modules
COPY src        ./src
COPY migrations ./migrations
COPY migrate.ts ./migrate.ts

EXPOSE 3000

CMD ["bun", "src/index.ts"]
