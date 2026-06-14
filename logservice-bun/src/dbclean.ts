import db from "$/db";

// Drops every base table in the configured database (including _migrations),
// returning it to an empty schema. Intended for dev/test resets — run
// `db:migrate` afterwards to rebuild, or use `db:reset` to do both.
const database = Bun.env.DB_NAME;

const rows = (await db`
    SELECT table_name AS name
    FROM information_schema.tables
    WHERE table_schema = ${database} AND table_type = 'BASE TABLE'
`) as { name: string }[];

if (rows.length === 0) {
    console.log(`[db:clean] no tables in '${database}'`);
} else {
    const list = rows.map((r) => `\`${r.name}\``).join(", ");
    await db.unsafe(`DROP TABLE IF EXISTS ${list}`);
    console.log(
        `[db:clean] dropped ${rows.length} table(s) from '${database}': ${rows
            .map((r) => r.name)
            .join(", ")}`,
    );
}

// best-effort close so the script can exit; Bun's MySQL db.end() may reject
// with ERR_MYSQL_CONNECTION_CLOSED, which is harmless here.
await db.end().catch(() => {});
