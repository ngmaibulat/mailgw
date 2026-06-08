import { runMigrations } from "./migrate";
import { withAuth } from "./middleware/auth";
import { withErrorHandling } from "./middleware/error";
import { rootRoute } from "./routes/root";
import {
    deliveryRoute, deliverySearchRoute,
    queueRoute,
    connectionRoute, connectionSearchRoute,
    transactionSearchRoute,
} from "./routes/api";

type Handler = (req: Request) => Response | Promise<Response>;

function handle(handler: Handler): Handler {
    return withAuth(withErrorHandling(handler));
}

const port = Number(Bun.env.PORT ?? 3000);

if (process.argv.includes("--migrate")) {
    try {
        await runMigrations();
    } catch (err) {
        console.error("[migrate] failed, aborting startup:", err);
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
    },
    fetch(_req) {
        return new Response("Resource does not exist\n", { status: 404 });
    },
});

console.log(`[app] port: ${port}`);
