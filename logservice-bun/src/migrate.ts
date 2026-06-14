import { SQL } from "bun";
import { readdir, readFile } from "fs/promises";
import { join } from "path";

const migrationsDir = join(import.meta.dir, "../migrations");

export async function runMigrations(): Promise<void> {
    const db = new SQL({
        host: Bun.env.DB_HOST,
        port: Number(Bun.env.DB_PORT ?? 3306),
        user: Bun.env.DB_USER,
        password: Bun.env.DB_PASS,
        database: Bun.env.DB_NAME,
    });

    try {
        await db.unsafe(`
            CREATE TABLE IF NOT EXISTS _migrations (
                id        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
                name      VARCHAR(255) NOT NULL UNIQUE,
                appliedAt DATETIME     NOT NULL
            )
        `);

        const applied = new Set(
            (await db.unsafe("SELECT name FROM _migrations") as { name: string }[])
                .map(r => r.name)
        );

        const files = (await readdir(migrationsDir))
            .filter(f => f.endsWith(".sql"))
            .sort();

        let count = 0;
        for (const file of files) {
            if (applied.has(file)) continue;
            const sql = await readFile(join(migrationsDir, file), "utf8");
            await db.unsafe(sql);
            await db.unsafe("INSERT INTO _migrations (name, appliedAt) VALUES (?, NOW())", [file]);
            console.log(`[migrate] apply ${file}`);
            count++;
        }

        if (count === 0) {
            console.log("[migrate] all migrations already applied");
        } else {
            console.log(`[migrate] ${count} migration(s) applied`);
        }
    } finally {
        await db.close();
    }
}
