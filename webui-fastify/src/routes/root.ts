import type { FastifyInstance } from "fastify";
import { asc } from "drizzle-orm";

import { db, gateways } from "../../db/index.ts";
import { type FleetSummary, summarize } from "../central/fleet.ts";
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

// Fleet health for the dashboard card.
//
// Only the columns the summary needs, so the home page does not pull every
// gateway's system info and bundle pointers to count statuses. Best-effort in
// the same spirit as the feed above: a broken query renders the page without
// the card instead of 500ing the one URL an operator always opens first.
async function fleetSummary(
    log: FastifyInstance["log"],
): Promise<{ summary: FleetSummary | null; unavailable: boolean }> {
    try {
        const rows = await db
            .select({
                id: gateways.id,
                name: gateways.name,
                hostname: gateways.hostname,
                status: gateways.status,
                last_seen: gateways.last_seen,
                apply_error: gateways.apply_error,
                restart_required: gateways.restart_required,
            })
            .from(gateways)
            .orderBy(asc(gateways.name));
        return { summary: summarize(rows), unavailable: false };
    } catch (err) {
        log.error({ err }, "cannot load the fleet summary for the dashboard");
        return { summary: null, unavailable: true };
    }
}

export default async function rootRoutes(fastify: FastifyInstance) {
    fastify.get("/", async (request, reply) => {
        // Independent of each other, so they run together rather than in
        // series: one is a logservice round trip and the other a DB query.
        const [feed, fleet] = await Promise.all([
            recentDeliveries(),
            fleetSummary(request.log),
        ]);
        return reply.view("index", { feed, fleet });
    });
}
