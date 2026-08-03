/**
 * Bounces, against a relay that refuses.
 *
 * # Why this is not a Go test
 *
 * internal/dsn pins the rendered notification against a golden file and
 * internal/queue/bounce_test.go covers when one is generated. Neither can show
 * the loop: a real relay answering 5xx, the gateway building a report, ROUTING
 * that report through the rule engine, and delivering it back to a real relay.
 *
 * TP-07's own preconditions say why this needs the fake: "A relay that can be
 * made to reject a recipient 5xx. A second MailHog will not do this — use a
 * small nc-scripted listener." This is that listener.
 *
 * Automates TP-07 steps 3-6 and 11-18.
 */

import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";

import {
    baseline,
    haveBinary,
    startGateway,
    startSink,
    type Gateway,
    type SinkMessage,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

const DSN_CONFIG =
    "dsn:\n" +
    "    enabled: true\n" +
    "    return: headers\n" +
    "    postmaster: postmaster@gw.test\n" +
    "    relay_group: Outbound\n" +
    "outbound:\n" +
    "    backoff: [1s]\n" +
    "    poll_interval: 1s\n";

/** A notification is a multipart/report; ordinary mail is not. */
const isReport = (m: SinkMessage) =>
    (m.headers["content-type"]?.[0] ?? "").includes("report-type=delivery-status");

/**
 * The machine-readable part of a report — the per-recipient blocks.
 *
 * Worth isolating: the third part quotes the ORIGINAL message's headers
 * verbatim, so a naive substring search over the whole body finds addresses the
 * report is deliberately not reporting on.
 */
function deliveryStatus(report: string): string {
    const start = report.indexOf("Content-Type: message/delivery-status");
    if (start < 0) return "";
    const end = report.indexOf("\r\n--", start);
    return report.slice(start, end < 0 ? undefined : end);
}

describe.skipIf(!enabled)("a relay that refuses permanently", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        // 550 at RCPT, so the refusal is per recipient and the gateway can tell
        // which addresses failed — the case a report exists to describe.
        sink = await startSink({
            rcpt: (addr) =>
                addr.includes("reject-me")
                    ? "550 5.1.1 <" + addr + "> no such user"
                    : "250 2.1.5 Recipient ok",
        });
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), serverExtra: DSN_CONFIG }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    afterEach(() => sink.reset());

    test("the sender gets a failure report naming the address", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "bounce-me",
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);

        // The envelope sender is null — this is what stops two mail systems
        // bouncing at each other for ever.
        expect(report.from).toBe("");
        expect(report.rcpts).toEqual(["sender@example.test"]);
        expect(report.data).toContain("Action: failed");
        expect(report.data).toContain("Final-Recipient: rfc822; reject-me@partner.test");
        // The relay's own words, quoted, so the sender can act on them.
        expect(report.data).toMatch(/Diagnostic-Code: smtp; 550/);
        // dsn.return: headers, so the original comes back as headers only.
        expect(report.data).toContain("text/rfc822-headers");
        expect(report.data).toContain("Subject: bounce-me");
    }, 60_000);

    test("one report covers several refused recipients, not three reports", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: ["reject-me-1@partner.test", "reject-me-2@partner.test", "reject-me-3@partner.test"],
            subject: "three-at-once",
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);
        for (const n of [1, 2, 3]) {
            expect(report.data).toContain(`reject-me-${n}@partner.test`);
        }

        // Rejections are collected once per attempt precisely so a message to
        // three bad addresses does not produce three notifications.
        await Bun.sleep(2000);
        expect(sink.messages.filter(isReport)).toHaveLength(1);
    }, 60_000);

    test("the notification's identity nests under the message it reports on", async () => {
        const c = await gw.smtp();
        const { uuid } = await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "nesting",
        });
        await c.quit();

        await sink.waitForMessage(isReport, 40_000);

        // A bounce for X.1.1 is X.1.1.<n>, a literal prefix extension, so
        // `WHERE uuid LIKE 'X%'` still finds the whole tree. A freshly minted
        // root would leave a Delivery row with no Connection and no Transaction.
        const report = sink.messages.find(isReport)!;
        expect(report.data).toContain(uuid!);
    }, 60_000);

    test("NOTIFY=NEVER silences one recipient without silencing the report", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: ["reject-me-quiet@partner.test", "reject-me-loud@partner.test"],
            subject: "notify-never",
            rcptParams: { "reject-me-quiet@partner.test": ["NOTIFY=NEVER"] },
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);

        // Scoped to the machine-readable part, not the whole report: the third
        // part quotes the ORIGINAL headers verbatim, and the original message's
        // To: legitimately names both addresses. Asserting over the whole body
        // would be asserting that the gateway rewrites the message it is
        // reporting on, which it must not.
        const status = deliveryStatus(report.data);
        expect(status).toContain("reject-me-loud@partner.test");
        // One abstaining recipient must not silence the report for the others —
        // which is exactly what makes NEVER different from suppressing the lot.
        expect(status).not.toContain("reject-me-quiet@partner.test");
    }, 60_000);

    test("ORCPT comes back xtext-encoded", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "orcpt",
            rcptParams: {
                "reject-me@partner.test": ["ORCPT=rfc822;sales+2Bq3@partner.test"],
            },
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);
        // The `+` is an xtext special. go-smtp decodes on the way in and exports
        // no encoder, so emitting it raw would give the receiving system a value
        // that decodes into something else.
        expect(report.data).toContain("Original-Recipient: rfc822; sales+2Bq3@partner.test");
    }, 60_000);

    test("RET=FULL overrides the configured headers-only return", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "ret-full",
            body: "THE-ORIGINAL-BODY-MARKER",
            mailParams: ["RET=FULL"],
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);
        expect(report.data).toContain("message/rfc822");
        expect(report.data).toContain("THE-ORIGINAL-BODY-MARKER");
    }, 60_000);

    test("ENVID comes back verbatim, and is absent when not supplied", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "envid",
            mailParams: ["ENVID=batch-2026-Q3"],
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);
        // The SENDER's identifier. This used to be answered with the gateway's
        // own uuid, which gave anybody matching on the field a confident
        // non-match against something that looked like a real answer.
        expect(report.data).toContain("Original-Envelope-Id: batch-2026-Q3");
    }, 60_000);

    test("a successful delivery earns no report at all", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "fine@partner.test",
            subject: "no-news",
        });
        await c.quit();

        await sink.waitForMessage((m) => m.data.includes("no-news"));
        // A success report nobody asked for is unsolicited mail.
        expect(sink.messages.filter(isReport)).toHaveLength(0);
    }, 60_000);

    test("NOTIFY=SUCCESS earns a 'relayed' report, never 'delivered'", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "fine@partner.test",
            subject: "tell-me",
            rcptParams: { "fine@partner.test": ["NOTIFY=SUCCESS"] },
        });
        await c.quit();

        const report = await sink.waitForMessage(isReport, 40_000);
        // "relayed", not "delivered", is the honest word: a relay accepting a
        // recipient says nothing about what happened to it afterwards, and this
        // gateway does not pass DSN parameters to the next hop.
        expect(report.data).toContain("Action: relayed");
        expect(report.data).not.toContain("Action: delivered");
        expect(report.data).toContain("Status: 2.0.0");
    }, 60_000);
});

describe.skipIf(!enabled)("a relay that refuses temporarily", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink({ rcpt: "451 4.3.0 try again later" });
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                serverExtra: DSN_CONFIG + "    max_lifetime: 3600s\n",
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("does not bounce — it defers", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "later@partner.test",
            subject: "tempfail",
        });
        await c.quit();

        // Wait long enough for at least one attempt to have failed.
        const deadline = Date.now() + 20_000;
        while (Date.now() < deadline) {
            const entries = await gw.ctl.queue();
            if (entries.some((e) => e.attempts >= 1)) break;
            await Bun.sleep(200);
        }

        // Only a 5xx bounces. A 4xx means "come back", and turning a transient
        // condition into a permanent rejection is what this guards against — it
        // is the same rule that keeps ruleset.DefaultAction's 451 from bouncing.
        expect(sink.messages.filter(isReport)).toHaveLength(0);
        const entries = await gw.ctl.queue();
        expect(entries.length).toBeGreaterThan(0);
        expect(entries[0].last_error).toBeTruthy();
    }, 60_000);
});

describe.skipIf(!enabled)("a bounce that itself cannot be delivered", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        // Everything is refused at END OF DATA, including the notification the
        // first refusal generates.
        //
        // At end-of-DATA rather than at RCPT deliberately: the sink records a
        // message only once its body has arrived, so refusing at RCPT would
        // make the report invisible here and the test would pass whether or not
        // one had been generated.
        sink = await startSink({ dataEnd: "550 5.6.0 content rejected" });
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                serverExtra: DSN_CONFIG + "    max_lifetime: 3600s\n",
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("is buried rather than bounced again", async () => {
        const c = await gw.smtp();
        await c.sendMail({
            from: "sender@example.test",
            to: "reject-me@partner.test",
            subject: "bounce-of-a-bounce",
        });
        await c.quit();

        // The first report is generated and refused.
        await sink.waitForMessage(isReport, 40_000);

        // Give the queue time to produce a second one, if it were going to.
        await Bun.sleep(5000);

        // Exactly one. This is how two mail systems bounce at each other for
        // ever, and the null sender is what stops it: Suppress(env) is true for
        // a notification, so bounceFailed counts the suppression instead of
        // generating a second report.
        expect(sink.messages.filter(isReport)).toHaveLength(1);

        // Counted, not silent — mailgw_dsn_suppressed_total is how an operator
        // sees that a sender was not told.
        const metrics = await gw.metrics();
        expect(metrics["mailgw_dsn_suppressed_total"] ?? 0).toBeGreaterThan(0);

        // And nothing is left behind. Note the refused notification is
        // COMPLETED, not buried: dead/ is the max_lifetime path, and an
        // envelope every recipient permanently rejected is done rather than
        // given up on. (TP-07 step 6 says "buried in dead/"; it is not.)
        expect(await gw.ctl.queue()).toEqual([]);
    }, 90_000);
});
