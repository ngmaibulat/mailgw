import { readdir, readFile } from "fs/promises";
import { join } from "path";
import db from "$/db";

const migrationsDir = join(import.meta.dir, "../migrations");

const MAX_ATTEMPTS = 10;
const RETRY_DELAY_MS = 2000;

// Wait for the database to accept connections before migrating. Retries up to
// MAX_ATTEMPTS times (each attempt is bounded by the pool's connectionTimeout),
// which covers the case where the DB is still starting up.
async function waitForDb(): Promise<void> {
    const target = `${Bun.env.DB_HOST ?? "?"}:${Bun.env.DB_PORT ?? 3306}`;

    for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
        try {
            await db`SELECT 1`;
            if (attempt > 1) {
                console.log(`[migrate] connected to ${target} on attempt ${attempt}`);
            }
            return;
        } catch (err) {
            const reason = err instanceof Error ? err.message : String(err);
            if (attempt === MAX_ATTEMPTS) {
                throw new Error(
                    `could not connect to the database at ${target} after ${MAX_ATTEMPTS} attempts — ` +
                        `is MariaDB running and reachable? (last error: ${reason})`,
                );
            }
            console.warn(
                `[migrate] database at ${target} not ready yet ` +
                    `(attempt ${attempt}/${MAX_ATTEMPTS}, retrying in ${RETRY_DELAY_MS / 1000}s)`,
            );
            await Bun.sleep(RETRY_DELAY_MS);
        }
    }
}

export async function runMigrations(): Promise<void> {
    await waitForDb();

    await db.unsafe(`
        CREATE TABLE IF NOT EXISTS _migrations (
            id        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
            name      VARCHAR(255) NOT NULL UNIQUE,
            appliedAt DATETIME     NOT NULL
        )
    `);

    const applied = new Set(
        ((await db.unsafe("SELECT name FROM _migrations")) as { name: string }[])
            .map((r) => r.name),
    );

    const files = (await readdir(migrationsDir))
        .filter((f) => f.endsWith(".sql"))
        .sort();

    let count = 0;
    for (const file of files) {
        if (applied.has(file)) continue;

        const sql = await readFile(join(migrationsDir, file), "utf8");
        try {
            await db.unsafe(sql);
            await db.unsafe(
                "INSERT INTO _migrations (name, appliedAt) VALUES (?, NOW())",
                [file],
            );
        } catch (err) {
            const reason = err instanceof Error ? err.message : String(err);
            throw new Error(`migration ${file} failed: ${reason}`);
        }
        console.log(`[migrate] apply ${file}`);
        count++;
    }

    console.log(
        count === 0
            ? "[migrate] all migrations already applied"
            : `[migrate] ${count} migration(s) applied`,
    );
}

// Allow running directly as a one-shot: `bun src/dbmigrate.ts` (db:migrate).
// Guarded so importing this module (e.g. from index.ts) has no side effect.
if (import.meta.main) {
    try {
        await runMigrations();
        process.exit(0);
    } catch (err) {
        const reason = err instanceof Error ? err.message : String(err);
        console.error(`[migrate] aborted: ${reason}`);
        process.exit(1);
    }
}
