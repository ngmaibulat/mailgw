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

export async function closeDb(): Promise<void> {
    await pool.end();
}
