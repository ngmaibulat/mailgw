import { Table, getTableName, is } from "drizzle-orm";
import { drizzle } from "drizzle-orm/mysql2";
import mysql from "mysql2/promise";

import * as schema from "./schema.ts";

// Lazy pool — no connection until the first query, so importing this module is
// side-effect-light (unlike the old Sequelize config that connected + ran a
// schema check + process.exit at import time).
const pool = mysql.createPool({
    host: process.env.DB_HOST,
    user: process.env.DB_USER,
    password: process.env.DB_PASS,
    database: process.env.DB_NAME,
});

export const db = drizzle(pool, { schema, mode: "default" });

// The transaction handle drizzle hands to a db.transaction() callback. It is a
// full MySqlDatabase, so every query builder works identically on it; spelling
// it structurally avoids importing MySqlTransaction with its four type
// arguments.
export type Tx = Parameters<Parameters<typeof db.transaction>[0]>[0];

// Either a pooled connection or a transaction. A helper that takes this can be
// called from inside or outside a transaction, which is what lets composeBundle
// be reused by deployBundle without a second code path.
export type DB = typeof db | Tx;

// Re-export the tables so callers get both `db` and the table refs from one
// import: `import { db, relays } from "../../db/index.ts"`.
export * from "./schema.ts";

// Fail fast at startup if the DB is unreachable. The webui does NOT create or
// migrate the schema — logservice owns the tables — so this only pings.
export async function assertDbConnection(): Promise<void> {
    const conn = await pool.getConnection();
    await conn.ping();
    conn.release();
}

// The tables this build queries, derived from schema.ts rather than listed by
// hand. Hardcoding "Users" would have been shorter and would have gone stale
// the first time a milestone added a table (M6 added one, M13 two) — and a
// console deployed ahead of its migration would then report "ready" and answer
// 500 instead of naming the table it is waiting for.
export function expectedTables(): string[] {
    return Object.values(schema)
        .filter((v) => is(v, Table))
        .map((t) => getTableName(t))
        .sort();
}

// Which of `want` the database does not have yet. One query, one round trip:
// this runs in a poll loop and a per-table probe would be 14 of them.
async function missingTables(want: string[]): Promise<string[]> {
    const placeholders = want.map(() => "?").join(",");
    const [rows] = await pool.query(
        `SELECT TABLE_NAME AS name FROM information_schema.TABLES
          WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (${placeholders})`,
        want,
    );
    // Compared case-insensitively: MariaDB's lower_case_table_names decides
    // whether `Users` comes back as it was created or folded, and which of
    // those happens is a property of the host, not of this schema.
    const have = new Set(
        (rows as Array<{ name: string }>).map((r) =>
            String(r.name).toLowerCase(),
        ),
    );
    return want.filter((t) => !have.has(t.toLowerCase()));
}

// How long waitForSchema waits, from the environment. Never throws: a
// malformed value warns and falls back, the same rule logservice-go's envInt
// follows — and here the alternative is worse than a bad number, because
// Number("soon") is NaN and a NaN deadline is a boot that never finishes.
// An explicit 0 does disable the wait; it is the one value that means
// something.
function schemaWaitTimeout(): number {
    const raw = process.env.SCHEMA_WAIT_TIMEOUT_MS;
    if (raw === undefined || raw === "") return 90_000;
    const ms = Number(raw);
    if (!Number.isFinite(ms) || ms < 0) {
        console.warn(
            `SCHEMA_WAIT_TIMEOUT_MS=${raw} is not a duration in ms — using 90000`,
        );
        return 90_000;
    }
    return ms;
}

// Block until logservice has migrated the schema this console reads.
//
// The console does not own these tables and does not create them — logservice
// applies the migrations, before it binds. Until M22 the ordering was compose's
// problem: a one-shot `db-migrator` service ran to completion and the webui
// gated on it. Removing that container moves the gate here, where it also holds
// for a console started by hand against a fresh database with no compose file
// in sight.
//
// Timing out is fatal to the caller by design: starting anyway means /login and
// /setup answering 500, which reads as a broken console rather than a stack
// that is still coming up. Under `restart: unless-stopped` an exit is a bounded
// retry that costs a few seconds.
export async function waitForSchema({
    timeoutMs = schemaWaitTimeout(),
    pollMs = 1000,
}: {
    timeoutMs?: number;
    pollMs?: number;
} = {}): Promise<void> {
    if (timeoutMs <= 0) return; // explicitly disabled

    const want = expectedTables();
    const deadline = Date.now() + timeoutMs;
    const startedAt = Date.now();
    let announced = false;
    let lastProgress = 0;

    for (;;) {
        const missing = await missingTables(want);
        if (missing.length === 0) {
            // Silent on the common path — every restart after the first.
            if (announced) {
                const secs = Math.round((Date.now() - startedAt) / 1000);
                console.log(`Schema: ready after ${secs}s`);
            }
            return;
        }

        if (!announced) {
            announced = true;
            lastProgress = Date.now();
            console.log(
                `Schema: waiting for logservice to migrate ${missing.length} of ${want.length} tables (${missing.join(", ")})`,
            );
        } else if (Date.now() - lastProgress >= 10_000) {
            lastProgress = Date.now();
            console.log(`Schema: still missing ${missing.join(", ")}`);
        }

        if (Date.now() + pollMs >= deadline) {
            throw new Error(
                `schema not migrated after ${Math.round(timeoutMs / 1000)}s — missing ${missing.join(", ")}. ` +
                    "logservice applies these migrations when it starts; check that it is running and healthy.",
            );
        }
        await new Promise((resolve) => setTimeout(resolve, pollMs));
    }
}

export async function closeDb(): Promise<void> {
    await pool.end();
}
