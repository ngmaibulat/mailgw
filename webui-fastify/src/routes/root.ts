import type { FastifyInstance } from "fastify";

import { search } from "../logservice.ts";

// A single delivery row as the dashboard feed needs it. logservice returns more
// columns; we only read these and tolerate them being absent.
interface DeliveryRow {
    dt?: string;
    sender?: string;
    rcpt_list?: string;
    host?: string;
    response?: string;
}

// Pull the most recent deliveries for the dashboard feed. logservice orders by
// `id DESC`, so a small limit gives us the newest rows. Best-effort: if
// logservice is unreachable we render the dashboard without the feed rather than
// failing the home page (the feed is informational, not load-bearing).
async function recentDeliveries(): Promise<{
    rows: DeliveryRow[];
    unavailable: boolean;
}> {
    try {
        const res = (await search(
            "/api/delivery",
            JSON.stringify({ limit: 8 }),
        )) as { records?: DeliveryRow[] };
        return { rows: res.records ?? [], unavailable: false };
    } catch {
        return { rows: [], unavailable: true };
    }
}

export default async function rootRoutes(fastify: FastifyInstance) {
    fastify.get("/", async (_request, reply) => {
        const feed = await recentDeliveries();
        return reply.view("index", { feed });
    });
}
