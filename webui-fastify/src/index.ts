import "./checkenv.ts"; // validates required env vars; must run before the DB-connecting imports below
import "./errhandler.ts";
import fs from "node:fs";

import { build } from "./app.ts";
import { assertDbConnection } from "../db/index.ts";

const port = Number(process.env.PORT) || 4000;

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
    logger: false,
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
