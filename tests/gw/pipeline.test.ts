/**
 * Accept → spool → deliver → audit, end to end, across a process boundary.
 *
 * # Why this is not a Go test
 *
 * The Go suite covers every stage of this chain and no test spans it.
 * `internal/smtpsrv/contract_test.go` stops at the spool; `internal/queue`'s
 * tests substitute the deliverer; `internal/deliver`'s drive the client
 * directly. The seam between them — queue.Runner handing real envelopes to a
 * real deliver.Client, against a listener that is not a Go test helper — has no
 * coverage anywhere.
 *
 * The audit trail is the same story: internal/events covers the retry ladder,
 * and nothing shows the three events actually crossing a socket in the shape a
 * logservice would receive them.
 */

import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";

import {
    baseline,
    haveBinary,
    startGateway,
    startLogSink,
    startSink,
    type Gateway,
    type LogSink,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

describe.skipIf(!enabled)("the mail path", () => {
    let gw: Gateway;
    let sink: Sink;
    let logs: LogSink;

    beforeAll(async () => {
        sink = await startSink();
        logs = startLogSink();
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), logging: logs.logging }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
        logs?.stop();
    });

    afterEach(() => {
        sink.reset();
        logs.reset();
    });

    test("a message reaches the relay with the body it was given", async () => {
        const c = await gw.smtp();
        const marker = "pipeline-body-" + Date.now();
        const { queued, uuid } = await c.sendMail({
            from: "sender@example.test",
            to: "rcpt@partner.test",
            subject: "pipeline",
            body: `${marker}\r\n.\r\nand a line that began with a dot`,
        });
        await c.quit();

        // The 250 is a deliberate SMTPError so its text can carry the id, and
        // tests/smtp scrapes exactly this shape.
        expect(queued.code).toBe(250);
        expect(queued.raw).toMatch(/queued/i);
        expect(uuid).toBeTruthy();

        const msg = await sink.waitForMessage((m) => m.data.includes(marker));
        expect(msg.from).toBe("sender@example.test");
        expect(msg.rcpts).toEqual(["rcpt@partner.test"]);

        // Dot-stuffing survives the round trip. The client stuffs, the gateway
        // unstuffs into the spool, re-stuffs on the way out, and the sink
        // unstuffs again — four places to lose a line that starts with a dot.
        expect(msg.data).toContain("\r\n.\r\nand a line that began with a dot");
    });

    test("the gateway stamps its own headers on the way out", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "rcpt@partner.test",
            subject: "headers",
        });
        await c.quit();

        const msg = await sink.waitForMessage();
        // The label that tells the two gateways apart in the log tables.
        expect(msg.headers["x-ngm-gateway"]).toEqual(["go"]);
        // Its own Received:, naming the configured hostname.
        expect(msg.headers["received"]?.[0]).toContain("gw.test");
        expect(msg.headers["subject"]).toEqual(["headers"]);
    });

    test("HELO cannot inject a header", async () => {
        const c = await gw.smtp();
        // The HELO name lands in the Received: header, so a CRLF in it would be
        // a header injection if it were not neutralised.
        await c.cmd("EHLO evil\\r\\nX-Injected: yes");
        await c.mail("sender@example.test");
        await c.rcpt("rcpt@partner.test");
        await c.cmd("DATA");
        await c.dataBody("Subject: injection\r\n\r\nbody\r\n");
        await c.quit();

        const msg = await sink.waitForMessage();
        expect(msg.headers["x-injected"]).toBeUndefined();
    });

    test("the three audit events share one uuid tree", async () => {
        const c = await gw.smtp();
        const { uuid } = await c.sendMail({
            from: "sender@example.test",
            to: "rcpt@partner.test",
            subject: "uuids",
        });
        await c.quit();

        const conn = await logs.waitFor("connection");
        const txn = await logs.waitFor("queue", (e) => e.uuid?.startsWith(conn.uuid));
        const delivery = await logs.waitFor("delivery", (e) => e.uuid?.startsWith(conn.uuid));

        // X / X.1 / X.1.1 — the hard contract the e2e DB assertions rely on,
        // because they find all three with `WHERE uuid LIKE 'X%'`.
        expect(txn.uuid).toBe(`${conn.uuid}.1`);
        expect(delivery.uuid).toBe(`${conn.uuid}.1.1`);

        // The scraper yields the CONNECTION id, not the transaction's: the 250
        // carries "(X.1)" and the regex captures X, leaving ".1" outside the
        // group. That is deliberate — it is what makes `uuid + "%"` match all
        // three tables — and it is the same shape internal/smtpsrv asserts.
        expect(uuid).toBe(conn.uuid);
    });

    test("every audit row is labelled with the gateway that wrote it", async () => {
        const c = await gw.smtp();
        await c.sendMail({ from: "sender@example.test", to: "rcpt@partner.test" });
        await c.quit();

        const conn = await logs.waitFor("connection");
        const txn = await logs.waitFor("queue");
        const delivery = await logs.waitFor("delivery");

        // Resolved once at bring-up. Unmanaged here, so it falls back to
        // server.hostname — which is what makes it stable across restarts.
        for (const e of [conn, txn, delivery]) {
            expect(e.gateway).toBe("gw.test");
        }
    });

    test("a delivery row names the rule that routed it", async () => {
        const c = await gw.smtp();
        await c.sendMail({ from: "sender@example.test", to: "rcpt@partner.test" });
        await c.quit();

        const delivery = await logs.waitFor("delivery");
        // Per recipient, because one envelope groups by relay group and can hold
        // recipients routed there by different rules.
        expect(delivery.route_rule).toBe("Default");
        expect(delivery.rcpt_accepted).toBe("rcpt@partner.test");
        // Single-valued, always: the contract the Haraka plugin broke by
        // comma-joining, which made every multi-recipient delivery 400 silently.
        expect(delivery.rcpt_list).toBe("rcpt@partner.test");
    });

    test("a connection that never reaches DATA still produces a row", async () => {
        const c = await gw.smtp();
        await c.ehlo("just-looking.test");
        await c.quit();

        // Posted from Logout when DATA was never reached. Without it a probe or
        // a refused sender leaves no trace at all.
        const conn = await logs.waitFor("connection", (e) => e.hello_name === "just-looking.test");
        expect(conn.remoteAddr).toBeTruthy();
        expect(conn.tran_count).toBe(0);
    });
});

describe.skipIf(!enabled)("when the log service refuses", () => {
    let gw: Gateway;
    let sink: Sink;
    let logs: LogSink;

    beforeAll(async () => {
        sink = await startSink();
        logs = startLogSink();
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                logging: logs.logging,
                // Replay often, so the test asserts on the loop rather than on
                // the clock. The shipped default is 5m.
                serverExtra: "events:\n    replay_interval: 1s\n",
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
        logs?.stop();
    });

    test("mail still flows, and the events spill to disk", async () => {
        // A 400 is terminal by design — an identical body cannot start passing —
        // so this uses a 500, which is the retry-then-spill path.
        logs.respond("/api/queue", 500);

        const c = await gw.smtp();
        const { queued } = await c.sendMail({
            from: "sender@example.test",
            to: "rcpt@partner.test",
            subject: "spill",
        });
        await c.quit();

        // The load-bearing property: the mail path never waits on the audit
        // trail, and never fails because of it.
        expect(queued.code).toBe(250);
        await sink.waitForMessage();

        // The spilled event is on disk and countable.
        const deadline = Date.now() + 30_000;
        let spilled = 0;
        while (Date.now() < deadline) {
            spilled = (await gw.metrics())["mailgw_events_spilled_total"] ?? 0;
            if (spilled > 0) break;
            await Bun.sleep(100);
        }
        expect(spilled).toBeGreaterThan(0);
    }, 60_000);

    test("and the replayer drains them once it recovers", async () => {
        logs.healthy();

        const deadline = Date.now() + 30_000;
        let replayed = 0;
        while (Date.now() < deadline) {
            replayed = (await gw.metrics())["mailgw_events_replayed_total"] ?? 0;
            if (replayed > 0) break;
            await Bun.sleep(200);
        }

        // A spilled event is parked, not lost — which is the whole difference
        // between mailgw_events_spilled_total and mailgw_events_dropped_total.
        expect(replayed).toBeGreaterThan(0);
        expect(logs.queues.length).toBeGreaterThan(0);
    }, 60_000);
});
