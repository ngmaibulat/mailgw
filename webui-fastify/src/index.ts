import "./checkenv.ts"; // validates required env vars; must run before the DB-connecting imports below
import "./errhandler.ts";

import { build } from "./app.ts";
import { ensureTlsPair } from "./tls.ts";
import { assertDbConnection, closeDb, waitForSchema } from "../db/index.ts";
import { purgeOldLogs } from "./middleware/logger.ts";
import { countUsers } from "./auth/users.ts";

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

// ...and then wait for logservice to have migrated the tables this console
// reads. Reachable is not the same as ready on a fresh volume: MariaDB answers
// as soon as it is up, while the schema arrives when logservice starts. This is
// the gate the `db-migrator` compose service used to be (M22).
try {
    await waitForSchema();
} catch (err) {
    console.error("Schema: FAIL —", (err as Error).message);
    process.exit(1);
}

// First-run hint: if there are no accounts, the UI is unusable until one is
// created. Point the operator at the one-time /setup page (or the CLI).
try {
    if ((await countUsers()) === 0) {
        console.warn(
            `No users exist yet — open https://localhost:${port}/setup to create the first admin user (or run: node create_user.ts <email> <password>).`,
        );
    }
} catch (err) {
    console.error("User count check failed:", (err as Error).message);
}

// Fastify supports HTTP/2 natively (unlike Express). `allowHTTP1: true` lets
// the same TLS port also serve plain HTTP/1.1 clients — ALPN negotiates h2
// when the client supports it and falls back to http/1.1 otherwise.
// Reads ./certs/server.{key,crt}, or mints a self-signed pair there if the
// directory holds none — so a fresh `docker compose up` serves TLS with no
// preparatory step, while a mounted pair is used exactly as given. TLS_DIR
// moves the directory; TLS_HOSTS adds SANs to a generated certificate.
const tls = ensureTlsPair(process.env.TLS_DIR || "./certs");

const app = await build({
    logger,
    http2: true,
    https: {
        allowHTTP1: true,
        key: tls.key,
        cert: tls.cert,
    },
});

try {
    await app.listen({ port, host: "0.0.0.0" });
    console.log(`webui listening on https://localhost:${port}`);
} catch (err) {
    console.error(err);
    process.exit(1);
}

// Audit-log retention. Purge once at boot, then every 6h. `LOG_RETENTION_DAYS`
// (default 30) tunes the window; 0 disables purging. Scheduled here (not in
// build()) so app.inject() tests don't touch the DB. Unref'd so it never holds
// the event loop open; cleared on shutdown.
const retentionDays = Number(process.env.LOG_RETENTION_DAYS ?? 30);
const runPurge = () =>
    purgeOldLogs(retentionDays).catch((err) =>
        console.error("log retention purge failed:", (err as Error).message),
    );
runPurge();
const purgeTimer = setInterval(runPurge, 6 * 60 * 60 * 1000);
purgeTimer.unref();

// Graceful shutdown: stop accepting connections, run Fastify onClose hooks
// (which clear the session-sweep timer), then close the DB pool. Docker/k8s
// send SIGTERM on stop; without this the process was killed abruptly.
let shuttingDown = false;
async function shutdown(signal: string): Promise<void> {
    if (shuttingDown) return;
    shuttingDown = true;
    console.log(`${signal} received — shutting down`);
    clearInterval(purgeTimer);
    try {
        await app.close();
        await closeDb();
        console.log("shutdown complete");
        process.exit(0);
    } catch (err) {
        console.error("error during shutdown:", (err as Error).message);
        process.exit(1);
    }
}

process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));
