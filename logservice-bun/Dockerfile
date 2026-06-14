FROM oven/bun:1-alpine AS deps

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production

# ---- final image ----
FROM oven/bun:1-alpine

WORKDIR /app

COPY --from=deps /app/node_modules ./node_modules
COPY src           ./src
COPY migrations    ./migrations
# tsconfig.json defines the "$/*" -> "src/*" path alias that Bun resolves at
# runtime; package.json identifies the project root. Both are required in the
# image or imports like "$/db" fail with "Cannot find module".
COPY package.json tsconfig.json ./

EXPOSE 3000

CMD ["bun", "src/index.ts"]
