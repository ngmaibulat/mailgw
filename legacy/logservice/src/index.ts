import { runMigrations } from "./dbmigrate";
import { withAuth } from "./middleware/auth";
import { withErrorHandling } from "./middleware/error";
import { rootRoute } from "./routes/root";
import {
    deliveryRoute, deliverySearchRoute,
    queueRoute,
    connectionRoute, connectionSearchRoute,
    transactionSearchRoute,
    hashlookupSearchRoute,
    filterMD5Route,
} from "./routes/api";

type Handler = (req: Request) => Response | Promise<Response>;

function handle(handler: Handler): Handler {
    return withAuth(withErrorHandling(handler));
}

const port = Number(Bun.env.PORT ?? 3000);

// `--migrate` is a one-shot: run migrations and exit, do NOT start the server.
// (The db-migrator container relies on this exiting so its dependents can start.)
if (process.argv.includes("--migrate")) {
    try {
        await runMigrations();
        process.exit(0);
    } catch (err) {
        const reason = err instanceof Error ? err.message : String(err);
        console.error(`[migrate] aborted: ${reason}`);
        process.exit(1);
    }
}

Bun.serve({
    port,
    routes: {
        "/": { GET: rootRoute },
        "/api/delivery":    { POST: handle(deliveryRoute),    GET: handle(deliverySearchRoute) },
        "/api/connection":  { POST: handle(connectionRoute),  GET: handle(connectionSearchRoute) },
        "/api/queue":       { POST: handle(queueRoute) },
        "/api/transaction": { GET:  handle(transactionSearchRoute) },
        "/api/hashlookup":  { GET:  handle(hashlookupSearchRoute) },
        "/filter/md5":      { POST: handle(filterMD5Route) },
    },
    fetch(_req) {
        return new Response("Resource does not exist\n", { status: 404 });
    },
});

console.log(`[app] port: ${port}`);
