/**
 * Reading the log tables.
 *
 * The audit path is asserted twice on purpose. tests/gw/pipeline.test.ts checks
 * the JSON the gateway posts, against a fake logservice; this checks that the
 * REAL logservice validated it, wrote it, and that the columns hold what the
 * grids read. Between them sits a schema, a Zod validator and a SQL migration,
 * none of which the gateway knows anything about.
 */

import { SQL } from "bun";

let db: SQL | null = null;

/**
 * The shared connection.
 *
 * Bun's SQL defaults to Postgres and HANGS on MariaDB without adapter:"mysql" —
 * a five-minute debugging session the first time, so it is stated here rather
 * than rediscovered.
 */
export function conn(): SQL {
    db ??= new SQL({
        adapter: "mysql",
        hostname: process.env.MAILGW_DB_HOST ?? "127.0.0.1",
        port: Number(process.env.MAILGW_DB_PORT ?? "3306"),
        username: process.env.MAILGW_DB_USER ?? "mailgw",
        password: process.env.MAILGW_DB_PASS ?? "P@ssw0rd",
        database: process.env.MAILGW_DB_NAME ?? "mailgw",
    });
    return db;
}

export async function close(): Promise<void> {
    try {
        // db.end() can throw on Bun's MySQL adapter; swallow it.
        await db?.end();
    } catch {
        /* ignore */
    }
    db = null;
}

/** True when MariaDB answers — used to skip rather than fail. */
export async function reachable(): Promise<boolean> {
    try {
        await conn()`SELECT 1`;
        return true;
    } catch {
        return false;
    }
}

/** Poll a query until it returns a row. Events land after the relay. */
export async function waitForRow<T = any>(
    run: () => Promise<T[]>,
    timeoutMs = 30_000,
): Promise<T | null> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
        const rows = await run().catch(() => [] as T[]);
        if (rows.length > 0) return rows[0];
        await Bun.sleep(500);
    }
    return null;
}
