/**
 * Opt-in full-pipeline tests: send via SMTP, then confirm the events reach the
 * logservice DB. Uses Bun's native SQL client (no mysql2/sequelize).
 *
 * Off by default. Enable against the dev stack with:
 *   MAILGW_DB_CHECK=1 bun test smtp.e2e
 *
 * DB connection defaults to the docker-compose dev stack; override via
 * MAILGW_DB_HOST / _PORT / _USER / _PASS / _NAME.
 */
import { describe, test, expect, beforeAll, afterAll } from "bun:test";
import { SQL } from "bun";
import { SmtpClient } from "../src/smtp";
import { SMTP_HOST, SMTP_PORT } from "../../stack/baseline.ts";
import { waitForGatewayReady } from "../../stack/ready.ts";

const ENABLED = process.env.MAILGW_DB_CHECK === "1";

const HOST = SMTP_HOST;
// 2525, not 25. Both are published by docker-compose.yaml, so a default of 25
// did not fail — it silently tested the other mapping.
const PORT = SMTP_PORT;
const FROM = process.env.SMTP_FROM ?? "me@ngm.dev";
const TO = process.env.SMTP_TO ?? "test@ngm.dev";

// Bun's SQL defaults to Postgres and will hang on MariaDB unless adapter:"mysql".
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

// Poll a query until it returns a row (events are written after the relay).
async function waitFor(run: () => Promise<any[]>, timeoutMs = 20_000) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
        const rows = await run();
        if (rows.length > 0) return rows[0];
        await Bun.sleep(1000);
    }
    return null;
}

// Send one message up front and reuse its uuid across the table assertions.
// Connection.uuid == <uuid>, Transaction == <uuid>.1, Delivery == <uuid>.1.1,
// so a "<uuid>%" prefix match finds the row in every table.
let like = "";

beforeAll(async () => {
    if (!ENABLED) return;
    // The published port answers a TCP connect whether or not the gateway has
    // bound anything behind it, so without this the first failure is
    // "connection closed by peer" from inside a beforeAll.
    await waitForGatewayReady();
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const { uuid } = await c.sendMail({ from: FROM, to: TO, subject: "Bun e2e" });
    await c.quit();
    expect(uuid).toBeTruthy();
    like = uuid + "%";
    // 240s, not Bun's default 5s: sendMail alone is allowed 30s for the DATA
    // reply, so the default budget guaranteed a generic timeout in place of
    // whatever actually went wrong.
}, 240_000);

afterAll(async () => {
    // db.end() can throw on Bun's MySQL adapter; swallow it.
    try {
        await db?.end();
    } catch {}
});

describe.skipIf(!ENABLED)("event persistence", () => {
    test("Connection row is recorded", async () => {
        const row = await waitFor(
            () => db!`SELECT * FROM Connection WHERE uuid LIKE ${like} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();
        expect(row.remoteAddr).toBeTruthy();
        // 45s: the poll above is allowed 20s, which is already four times
        // Bun's default per-test budget.
    }, 45_000);

    test("Transaction row is recorded with the sender", async () => {
        const row = await waitFor(
            () => db!`SELECT * FROM Transaction WHERE uuid LIKE ${like} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();
        expect(row.sender).toBe(FROM);
    }, 45_000);

    test("Delivery row is recorded as queued", async () => {
        const row = await waitFor(
            () => db!`SELECT * FROM Delivery WHERE uuid LIKE ${like} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();
        expect(row.rcpt_accepted).toBe(TO);
        expect(row.response).toMatch(/queued|ok/i);
    }, 45_000);
});
