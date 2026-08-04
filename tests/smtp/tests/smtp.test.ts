/**
 * Bun-native SMTP integration tests for the running mailgw.
 *
 * Requires the stack up (`docker compose up -d`). Run from client/:
 *   bun test
 *   SMTP_HOST=… SMTP_PORT=… bun test
 *
 * Tests that complete a DATA phase perform a real relayed send (like swaks.sh).
 */
import { describe, test, expect, beforeAll } from "bun:test";
import { SmtpClient } from "../src/smtp";
import { SMTP_HOST, SMTP_PORT } from "../../stack/baseline.ts";
import { waitForGatewayReady } from "../../stack/ready.ts";

const HOST = SMTP_HOST;
// 2525, not 25. Both are published by docker-compose.yaml, so a default of 25
// did not fail — it silently tested the other mapping.
const PORT = SMTP_PORT;
const FROM = process.env.SMTP_FROM ?? "me@ngm.dev";
const TO = process.env.SMTP_TO ?? "test@ngm.dev";
const TO2 = process.env.SMTP_TO2 ?? "second@ngm.dev";

/** Open a connection and consume the 220 greeting. */
async function session(): Promise<SmtpClient> {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    return c;
}

beforeAll(async () => {
    try {
        // A poll, not a one-shot probe. This file asserts against the deployed
        // `e2e-server` profile — the EHLO keywords, the banner — so it needs the
        // gateway to be configured, not merely to answer a socket.
        await waitForGatewayReady();
    } catch (e) {
        throw new Error(
            `Cannot reach a provisioned mailgw at ${HOST}:${PORT}. Start the stack ` +
                `first (docker compose up -d && pnpm provision). Cause: ${(e as Error).message}`,
        );
    }
    // 240s, not Bun's default 5s: waitForGatewayReady is allowed 90s, and a
    // budget below the wait it contains can only ever report "timed out".
}, 240_000);

describe("connection & greeting", () => {
    test("greets with 220 on connect", async () => {
        const c = await SmtpClient.open(HOST, PORT);
        const greeting = await c.greeting();
        expect(greeting.code).toBe(220);
        expect(greeting.raw).toMatch(/ESMTP|Haraka/i);
        await c.quit();
    }, 15_000);
});

describe("EHLO / HELO", () => {
    test("EHLO returns 250", async () => {
        const c = await session();
        expect((await c.cmd("EHLO bun-smtp-test")).code).toBe(250);
        await c.quit();
    }, 15_000);

    test("EHLO advertises expected ESMTP capabilities", async () => {
        const c = await session();
        const caps = (await c.cmd("EHLO bun-smtp-test")).lines.join("\n");
        for (const cap of ["SIZE", "PIPELINING", "8BITMIME", "SMTPUTF8"]) {
            expect(caps).toMatch(new RegExp(cap, "i"));
        }
        await c.quit();
    }, 15_000);

    test("HELO (legacy) is accepted", async () => {
        const c = await session();
        expect((await c.cmd("HELO bun-smtp-test")).code).toBe(250);
        await c.quit();
    }, 15_000);
});

describe("session commands", () => {
    test("NOOP returns 250", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd("NOOP")).code).toBe(250);
        await c.quit();
    }, 15_000);

    test("RSET returns 250", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd("RSET")).code).toBe(250);
        await c.quit();
    }, 15_000);

    test("QUIT returns 221", async () => {
        const c = await session();
        const bye = await c.cmd("QUIT");
        expect(bye.code).toBe(221);
    }, 15_000);
});

describe("command sequencing", () => {
    test("MAIL FROM before EHLO/HELO is rejected", async () => {
        const c = await session();
        expect((await c.cmd(`MAIL FROM:<${FROM}>`)).code).toBeGreaterThanOrEqual(500);
        await c.quit();
    }, 15_000);

    test("RCPT TO before MAIL FROM is rejected", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd(`RCPT TO:<${TO}>`)).code).toBeGreaterThanOrEqual(500);
        await c.quit();
    }, 15_000);

    test("DATA before RCPT is rejected", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        await c.cmd(`MAIL FROM:<${FROM}>`);
        expect((await c.cmd("DATA")).code).toBeGreaterThanOrEqual(500);
        await c.quit();
    }, 15_000);

    test("unknown command is rejected", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd("FROBNICATE please")).code).toBeGreaterThanOrEqual(500);
        await c.quit();
    }, 15_000);

    test("RSET clears an in-progress transaction", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd(`MAIL FROM:<${FROM}>`)).code).toBe(250);
        expect((await c.cmd("RSET")).code).toBe(250);
        // After RSET the sender is forgotten, so RCPT must fail.
        expect((await c.cmd(`RCPT TO:<${TO}>`)).code).toBeGreaterThanOrEqual(500);
        await c.quit();
    }, 15_000);
});

describe("envelope", () => {
    test("null sender (MAIL FROM:<>) is accepted", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        expect((await c.cmd("MAIL FROM:<>")).code).toBe(250);
        expect((await c.cmd(`RCPT TO:<${TO}>`)).code).toBe(250);
        await c.quit();
    }, 15_000);

    test("multiple recipients are accepted", async () => {
        const c = await session();
        await c.cmd("EHLO bun-smtp-test");
        await c.cmd(`MAIL FROM:<${FROM}>`);
        expect((await c.cmd(`RCPT TO:<${TO}>`)).code).toBe(250);
        expect((await c.cmd(`RCPT TO:<${TO2}>`)).code).toBe(250);
        await c.quit();
    }, 15_000);
});

describe("delivery", () => {
    test("accepts and queues a routable message", async () => {
        const c = await session();
        const { queued, uuid } = await c.sendMail({ from: FROM, to: TO });
        expect(queued.code).toBe(250);
        expect(queued.raw).toMatch(/queued/i);
        expect(uuid).toBeTruthy();
        await c.quit();
    }, 60_000);

    test("handles two messages on one connection", async () => {
        const c = await session();
        const first = await c.sendMail({ from: FROM, to: TO, subject: "Bun msg 1" });
        const second = await c.sendMail({ from: FROM, to: TO, subject: "Bun msg 2" });
        expect(first.queued.code).toBe(250);
        expect(second.queued.code).toBe(250);
        // Same connection => same base UUID, but distinct transaction queue ids.
        expect(first.queued.raw).not.toBe(second.queued.raw);
        await c.quit();
    }, 60_000);
});
