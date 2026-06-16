import "./checkenv.ts"; // validates required env vars; must run before the DB-connecting imports below
import "./errhandler.ts";
import fs from "node:fs";

import { build } from "./app.ts";
import { assertDbConnection } from "../db/index.ts";

const port = Number(process.env.PORT) || 4000;

// Fastify's built-in request logging (pino) is enabled everywhere EXCEPT
// production: in prod the audit trail lives in the `logs`/`exceptions` tables
// (src/middleware/logger.ts, src/errhandler.ts), so the extra stdout noise is
// off by default. Outside prod it gives readable request/error logs during dev,
// pretty-printed via pino-pretty (a devDep — absent from the prod image, which
// never enables the logger anyway). `LOG_LEVEL` (default "info") tunes verbosity.
const isProd = process.env.NODE_ENV === "production";
const logger = isProd
    ? false
    : {
          level: process.env.LOG_LEVEL || "info",
          transport: {
              target: "pino-pretty",
              options: { translateTime: "HH:MM:ss Z", ignore: "pid,hostname" },
          },
      };

// Fail fast if the DB is unreachable (the webui does not create/migrate schema).
try {
    await assertDbConnection();
    console.log("DB Connection: OK");
} catch (err) {
    console.error("DB Connection: FAIL —", (err as Error).message);
    process.exit(1);
}

// Fastify supports HTTP/2 natively (unlike Express). `allowHTTP1: true` lets
// the same TLS port also serve plain HTTP/1.1 clients — ALPN negotiates h2
// when the client supports it and falls back to http/1.1 otherwise.
const app = await build({
    logger,
    http2: true,
    https: {
        allowHTTP1: true,
        key: fs.readFileSync("./certs/server.key"),
        cert: fs.readFileSync("./certs/server.crt"),
    },
});

try {
    await app.listen({ port, host: "0.0.0.0" });
    console.log(`webui listening on https://localhost:${port}`);
} catch (err) {
    console.error(err);
    process.exit(1);
}
