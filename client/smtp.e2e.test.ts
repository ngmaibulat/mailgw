/**
 * Opt-in full-pipeline test: send via SMTP, then confirm the event reaches the
 * logservice DB. Uses Bun's native SQL client (no mysql2/sequelize).
 *
 * Off by default. Enable against the dev stack with:
 *   MAILGW_DB_CHECK=1 bun test smtp.e2e.test.ts
 *
 * DB connection defaults to the docker-compose dev stack; override via
 * MAILGW_DB_HOST / _PORT / _USER / _PASS / _NAME.
 */
import { test, expect, afterAll } from "bun:test";
import { SQL } from "bun";
import { SmtpClient } from "./smtp";

const ENABLED = process.env.MAILGW_DB_CHECK === "1";

const HOST = process.env.SMTP_HOST ?? "127.0.0.1";
const PORT = Number(process.env.SMTP_PORT ?? "25");
const FROM = process.env.SMTP_FROM ?? "me@ngm.dev";
const TO = process.env.SMTP_TO ?? "test@ngm.dev";

// Bun's SQL defaults to Postgres and will hang on a MySQL/MariaDB server unless
// adapter:"mysql" is set explicitly.
const db = ENABLED
    ? new SQL({
          adapter: "mysql",
          hostname: process.env.MAILGW_DB_HOST ?? "127.0.0.1",
          port: Number(process.env.MAILGW_DB_PORT ?? "3306"),
          username: process.env.MAILGW_DB_USER ?? "mailgw",
          password: process.env.MAILGW_DB_PASS ?? "P@ssw0rd",
          database: process.env.MAILGW_DB_NAME ?? "mailgw",
      })
    : null;

afterAll(async () => {
    // db.end() can throw on Bun's MySQL adapter; swallow it.
    try {
        await db?.end();
    } catch {}
});

test.skipIf(!ENABLED)("send → Delivery row persisted in the DB", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const { uuid } = await c.sendMail({ from: FROM, to: TO, subject: "Bun e2e" });
    await c.quit();
    expect(uuid).toBeTruthy();

    // Delivery is recorded after the relay completes; poll briefly.
    const deadline = Date.now() + 20_000;
    let rows: { uuid: string; response: string }[] = [];
    while (Date.now() < deadline) {
        rows = await db!`
            SELECT uuid, response FROM Delivery
            WHERE uuid LIKE ${uuid + "%"}
            ORDER BY id DESC LIMIT 1
        `;
        if (rows.length > 0) break;
        await Bun.sleep(1000);
    }

    expect(rows.length).toBe(1);
    expect(rows[0].response).toMatch(/queued|ok/i);
});
