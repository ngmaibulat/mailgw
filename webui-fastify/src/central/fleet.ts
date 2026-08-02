import { inArray } from "drizzle-orm";

import { db, gatewayMetrics } from "../../db/index.ts";

// How often a gateway reports. It polls on this interval (pollInterval in
// mailgw-go/cmd/mailgw-go/agent.go) with ±10% jitter, and reports on every
// poll.
const POLL_INTERVAL_MS = 15_000;

// A gateway is stale once it has missed roughly three polls.
//
// Three rather than one: a single missed poll is normal — jitter, a slow
// request, a restart — and flagging it would make the fleet view cry wolf often
// enough that operators stop reading it. Three missed polls is 45 seconds of
// silence, which is not something a healthy node does.
export const STALE_AFTER_MS = POLL_INTERVAL_MS * 3;

export function isStale(
    lastSeen: Date | null | undefined,
    now = Date.now(),
): boolean {
    // Never seen is not stale — it is a gateway that registered and has not
    // reported yet, which the "pending" status already says more clearly.
    if (!lastSeen) return false;
    return now - lastSeen.getTime() > STALE_AFTER_MS;
}

// The counters the console renders. The gateway reports many more; these are
// the ones that answer "is this box healthy and is it doing anything?".
export interface GatewaySnapshot {
    queueReady: number;
    queueInflight: number;
    queueQuarantine: number;
    queueDead: number;
    messagesAccepted: number;
    delivered: number;
    deferred: number;
    bounced: number;
    connectionsDenied: number;
    updatedAt: Date | null;
}

// Keys are a contract with mailgw-go/internal/obs, which has a golden test
// pinning them. Anything missing reads as 0 — a mixed-version fleet is normal
// and an older gateway simply reports fewer counters.
function num(m: Record<string, unknown>, key: string): number {
    const v = m[key];
    return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

// Latest snapshot per gateway id, for the ids given.
//
// Best-effort per row: a snapshot that will not parse is dropped rather than
// failing the page. The column holds whatever a gateway sent, so a malformed
// value is a gateway bug and must not take the fleet view down with it.
export async function metricsFor(
    ids: number[],
): Promise<Map<number, GatewaySnapshot>> {
    const out = new Map<number, GatewaySnapshot>();
    if (ids.length === 0) return out;

    const rows = await db
        .select()
        .from(gatewayMetrics)
        .where(inArray(gatewayMetrics.gateway_id, ids));

    for (const row of rows) {
        let m: Record<string, unknown>;
        try {
            const parsed = JSON.parse(row.metrics);
            if (typeof parsed !== "object" || parsed === null) continue;
            m = parsed as Record<string, unknown>;
        } catch {
            continue;
        }

        out.set(row.gateway_id, {
            queueReady: num(m, "queue_ready"),
            queueInflight: num(m, "queue_inflight"),
            queueQuarantine: num(m, "queue_quarantine"),
            queueDead: num(m, "queue_dead"),
            messagesAccepted: num(m, "msg_accepted"),
            delivered: num(m, "deliver_ok"),
            deferred: num(m, "deliver_deferred"),
            bounced: num(m, "deliver_bounced"),
            connectionsDenied: num(m, "conn_denied"),
            updatedAt: row.updated_at ?? null,
        });
    }
    return out;
}

// A gateway row as the fleet views need it: enough to decide whether it is
// healthy, without loading the whole record.
export interface FleetRow {
    id: number;
    name: string | null;
    hostname: string | null;
    status: string;
    last_seen: Date | null;
    apply_error: string | null;
    restart_required: boolean;
}

// What the dashboard card shows.
//
// Deliberately counts rather than lists, except for the things that need a
// human: an operator scanning the home page needs to know whether to go and
// look, and at what.
export interface FleetSummary {
    total: number;
    pending: number;
    approved: number;
    other: number;
    stale: FleetRow[];
    errored: FleetRow[];
    needRestart: FleetRow[];
    // True when anything here needs attention, so the template has one thing to
    // branch on rather than four.
    healthy: boolean;
}

export function summarize(rows: FleetRow[], now = Date.now()): FleetSummary {
    const pending = rows.filter((r) => r.status === "pending");
    const approved = rows.filter((r) => r.status === "approved");

    // Staleness is only meaningful for a gateway that is supposed to be
    // talking to us. A rejected or revoked one is silent by design, and
    // flagging it would be noise that never clears.
    const stale = approved.filter((r) => isStale(r.last_seen, now));
    const errored = rows.filter((r) => !!r.apply_error);
    const needRestart = rows.filter((r) => r.restart_required);

    return {
        total: rows.length,
        pending: pending.length,
        approved: approved.length,
        other: rows.length - pending.length - approved.length,
        stale,
        errored,
        needRestart,
        healthy:
            pending.length === 0 &&
            stale.length === 0 &&
            errored.length === 0 &&
            needRestart.length === 0,
    };
}
