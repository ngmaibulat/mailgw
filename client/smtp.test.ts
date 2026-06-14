/**
 * Bun-native SMTP integration tests for the running mailgw.
 *
 * Requires the stack to be up (`docker compose up -d`). Run with:
 *   bun test                       # from client/
 *   SMTP_HOST=… SMTP_PORT=… bun test
 *
 * Note: the "accepts and queues" test performs a real relayed send (same as
 * swaks.sh), so each run delivers a message via the configured relay.
 */
import { test, expect, beforeAll } from "bun:test";
import { SmtpClient } from "./smtp";

const HOST = process.env.SMTP_HOST ?? "127.0.0.1";
const PORT = Number(process.env.SMTP_PORT ?? "25");
const FROM = process.env.SMTP_FROM ?? "me@ngm.dev";
const TO = process.env.SMTP_TO ?? "test@ngm.dev";

beforeAll(async () => {
    // Fail fast with a clear message if the gateway isn't reachable.
    try {
        const c = await SmtpClient.open(HOST, PORT);
        await c.greeting();
        await c.quit();
    } catch (e) {
        throw new Error(
            `Cannot reach mailgw at ${HOST}:${PORT}. Start the stack first ` +
                `(docker compose up -d). Cause: ${(e as Error).message}`,
        );
    }
});

test("greets with 220 on connect", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    const greeting = await c.greeting();
    expect(greeting.code).toBe(220);
    expect(greeting.raw).toMatch(/ESMTP|Haraka/i);
    await c.quit();
});

test("EHLO advertises ESMTP capabilities", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const ehlo = await c.cmd("EHLO bun-smtp-test");
    expect(ehlo.code).toBe(250);
    // Haraka advertises SIZE among others.
    expect(ehlo.lines.some((l) => /SIZE/i.test(l))).toBe(true);
    await c.quit();
});

test("accepts and queues a routable message", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const { queued, uuid } = await c.sendMail({ from: FROM, to: TO });
    expect(queued.code).toBe(250);
    expect(queued.raw).toMatch(/queued/i);
    expect(uuid).toBeTruthy();
    await c.quit();
});

test("rejects MAIL FROM before EHLO/HELO", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const reply = await c.cmd(`MAIL FROM:<${FROM}>`);
    // Haraka requires a greeting first; expect a 5xx (or 503 bad sequence).
    expect(reply.code).toBeGreaterThanOrEqual(500);
    await c.quit();
});

test("rejects an unknown command", async () => {
    const c = await SmtpClient.open(HOST, PORT);
    await c.greeting();
    const reply = await c.cmd("FROBNICATE please");
    expect(reply.code).toBeGreaterThanOrEqual(500);
    await c.quit();
});
