import { SQL } from "bun:sql";

const db = new SQL({
    host: Bun.env.DB_HOST,
    port: Number(Bun.env.DB_PORT ?? 3306),
    user: Bun.env.DB_USER,
    password: Bun.env.DB_PASS,
    database: Bun.env.DB_NAME,
});

export default db;
