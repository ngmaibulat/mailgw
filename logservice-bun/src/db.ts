import { SQL } from "bun";

const db = new SQL({
    // Bun's SQL defaults to the Postgres driver; this is MariaDB, so pin the
    // MySQL adapter explicitly (it handles MariaDB).
    adapter: "mysql",
    host: Bun.env.DB_HOST,
    port: Number(Bun.env.DB_PORT ?? 3306),
    user: Bun.env.DB_USER,
    password: Bun.env.DB_PASS,
    database: Bun.env.DB_NAME,
});

export default db;
