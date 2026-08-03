/**
 * A message through the real stack: the shipped image, the compose network,
 * MailHog and MariaDB.
 *
 * # Why this is not a Go test, and not a Tier-B one either
 *
 * Tier B proves the gateway's behaviour with fakes it controls. This proves the
 * two things fakes cannot:
 *
 *   - a third-party MTA, written by somebody else, accepts what the gateway
 *     produces, and
 *   - the audit events survive the REAL logservice — its Zod validator, its
 *     SQL migrations and the columns the log viewers read.
 *
 * Between the gateway's JSON and a row in MariaDB sits a schema the gateway
 * knows nothing about. This is the only test that crosses it.
 *
 * Replaces tests/smtp/tests/smtp.e2e.test.ts, and is NOT behind a flag: it
 * needs no more than the stack the rest of tests/ already needs, and
 * MAILGW_DB_CHECK meant "this never runs".
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import { SmtpClient } from "../harness/smtp.ts";
import { SMTP_PORT } from "./baseline.ts";
import { close, conn, reachable as dbReachable, waitForRow } from "./db.ts";
import { header, reachable as mailhogReachable, waitForMessage } from "./mailhog.ts";

const HOST = process.env.SMTP_HOST ?? "127.0.0.1";
const FROM = process.env.SMTP_FROM ?? "me@ngm.dev";
const TO = process.env.SMTP_TO ?? "test@ngm.dev";

/** Skip rather than fail when the stack is not up: `bun test tests/` must work. */
const stackUp = (await mailhogReachable()) && (await dbReachable());
if (!stackUp) {
    console.error(
        "\n[stack] skipping tests/stack/delivery.test.ts: MailHog or MariaDB is not reachable.\n" +
            "  docker compose up -d && pnpm provision\n",
    );
}

// One message, sent once, asserted from three directions.
let subject = "";
let uuid = "";

describe.skipIf(!stackUp)("a message through the whole stack", () => {
    beforeAll(async () => {
        subject = `stack-e2e-${crypto.randomUUID()}`;

        const c = await SmtpClient.open(HOST, SMTP_PORT);
        await c.greeting();
        const sent = await c.sendMail({ from: FROM, to: TO, subject });
        await c.quit();

        expect(sent.uuid).toBeTruthy();
        uuid = sent.uuid!;
    }, 60_000);

    afterAll(async () => {
        await close();
    });

    test("the client is told it was queued, with a scrapeable id", () => {
        // tests/smtp/src/smtp.ts:143 scrapes exactly this. The 250 is a
        // deliberate SMTPError so its text can carry the id.
        expect(uuid).toMatch(/^[0-9A-F-]+$/i);
    });

    test("MailHog holds it, stamped by this gateway", async () => {
        const msg = await waitForMessage(
            (m) => header(m, "Subject") === subject,
            60_000,
        );

        // The header that tells the two gateways apart in the log tables, and
        // the Received: line the gateway prepends.
        expect(header(msg, "X-NGM-Gateway")).toBe("go");
        expect(header(msg, "Received")).toBeTruthy();
        expect(msg.Raw.To).toContain(TO);
    }, 90_000);

    test("a Connection row is recorded", async () => {
        const row = await waitForRow(
            () =>
                conn()`SELECT * FROM Connection WHERE uuid = ${uuid} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();
        expect(row.remoteAddr).toBeTruthy();
        // Resolved once at bring-up: the gateway_uid under Central Management.
        expect(row.gateway).toBeTruthy();
    }, 60_000);

    test("a Transaction row is recorded, one level down the uuid tree", async () => {
        const row = await waitForRow(
            () =>
                conn()`SELECT * FROM Transaction WHERE uuid = ${uuid + ".1"} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();
        expect(row.sender).toBe(FROM);
        expect(row.gateway).toBeTruthy();
    }, 60_000);

    test("a Delivery row is recorded, naming the rule that routed it", async () => {
        const row = await waitForRow(
            () =>
                conn()`SELECT * FROM Delivery WHERE uuid = ${uuid + ".1.1"} ORDER BY id DESC LIMIT 1`,
        );
        expect(row).not.toBeNull();

        // Single-valued, always. The Haraka plugin comma-joined this and every
        // multi-recipient delivery 400'd — invisibly, because the POST was
        // fire-and-forget with no response check.
        expect(row.rcpt_accepted).toBe(TO);
        expect(row.response).toMatch(/queued|ok/i);

        // M6's per-recipient column. It is on Delivery and not on Transaction
        // because one envelope groups by relay group and can hold recipients
        // routed there by different rules.
        expect(row.route_rule).toBeTruthy();
    }, 60_000);

    test("all three rows are found by the prefix query the viewers use", async () => {
        // X / X.1 / X.1.1 is a hard contract precisely so this works.
        const rows = await conn()`
            SELECT 'conn' AS t, uuid FROM Connection  WHERE uuid LIKE ${uuid + "%"}
            UNION ALL
            SELECT 'txn',        uuid FROM Transaction WHERE uuid LIKE ${uuid + "%"}
            UNION ALL
            SELECT 'del',        uuid FROM Delivery    WHERE uuid LIKE ${uuid + "%"}`;
        expect(rows.map((r: { t: string }) => r.t).sort()).toEqual(["conn", "del", "txn"]);
    }, 60_000);
});
