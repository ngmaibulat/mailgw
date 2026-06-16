import type {
    FastifyReply,
    FastifyRequest,
    HookHandlerDoneFunction,
} from "fastify";
import { lt } from "drizzle-orm";

import { db, logs } from "../../db/index.ts";

// Fastify `onRequest` hook — writes one row per request to the Logs table.
// Only columns that exist in the `logs` schema are included (Sequelize silently
// dropped unknown attributes; Drizzle would reject them).
// Low-value, high-frequency requests we don't want flooding the Logs table.
// The w2ui grids auto-refresh by polling GET /api/* (read-only proxy reads), so
// those would otherwise dominate the audit log. Page navigation and config
// mutations (POST /config/*) are still recorded.
function isNoise(request: FastifyRequest): boolean {
    const path = request.url.split("?")[0];
    if (path === "/favicon.ico") return true;
    if (request.method === "GET" && path.startsWith("/api/")) return true;
    return false;
}

export function logger(
    request: FastifyRequest,
    _reply: FastifyReply,
    done: HookHandlerDoneFunction,
): void {
    if (isNoise(request)) {
        done();
        return;
    }

    const h = request.headers;
    const row = {
        url: `${request.protocol}://${h.host ?? ""}${request.url}`,
        path: request.url.split("?")[0],
        query: request.url,
        method: request.method,
        src_ip: request.ip,
        src_port: request.socket.remotePort,
        referer: (h.referer ?? h.referrer) as string | undefined,
        userAgent: h["user-agent"],
        origin: h.origin,
        user: "-",
    };

    // Fire-and-forget by design: an audit-write failure must never break the
    // request it's recording. The catch is logged with context (it used to log
    // the bare error), but failures are still not retried or surfaced.
    Promise.resolve(db.insert(logs).values(row)).catch((err) =>
        console.error("audit log write failed:", (err as Error).message),
    );

    if (process.env.NODE_ENV === "development") {
        console.log(row);
    }

    done();
}

// Delete audit rows older than `retentionDays` so the Logs table doesn't grow
// without bound. The webui doesn't own the schema, but it owns the writes, so
// it owns the cleanup. `retentionDays <= 0` disables purging (keep everything).
// Scheduled from src/index.ts.
export async function purgeOldLogs(retentionDays: number): Promise<number> {
    if (retentionDays <= 0) {
        return 0;
    }
    const cutoff = new Date(Date.now() - retentionDays * 24 * 60 * 60 * 1000);
    const result = await db.delete(logs).where(lt(logs.createdAt, cutoff));
    // drizzle/mysql2 returns [ResultSetHeader, FieldPacket[]]; the header
    // carries affectedRows.
    const [header] = result as unknown as [{ affectedRows?: number }];
    return header?.affectedRows ?? 0;
}
