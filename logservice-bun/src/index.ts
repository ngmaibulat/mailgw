import { runMigrations } from "./migrate";
import { withAuth } from "./middleware/auth";
import { rootRoute } from "./routes/root";
import {
    deliveryRoute, deliverySearchRoute,
    queueRoute,
    connectionRoute, connectionSearchRoute,
    transactionSearchRoute,
} from "./routes/api";

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
        "/api/delivery":    { POST: withAuth(deliveryRoute),    GET: withAuth(deliverySearchRoute) },
        "/api/connection":  { POST: withAuth(connectionRoute),  GET: withAuth(connectionSearchRoute) },
        "/api/queue":       { POST: withAuth(queueRoute) },
        "/api/transaction": { GET:  withAuth(transactionSearchRoute) },
    },
    fetch(_req) {
        return new Response("Resource does not exist\n", { status: 404 });
    },
});

console.log(`[app] port: ${port}`);
