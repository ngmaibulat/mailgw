# Pin the Bun version (and stay on the lean Alpine base). Bump deliberately.
ARG BUN_VERSION=1.3.14

FROM oven/bun:${BUN_VERSION}-alpine AS deps

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production

# ---- final image ----
FROM oven/bun:${BUN_VERSION}-alpine

# Patch OS packages (openssl, etc.) on top of the pinned base image so we don't
# ship known-fixed CVEs even when the base tag lags behind Alpine updates.
RUN apk --no-cache -U upgrade

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
