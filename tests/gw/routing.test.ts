/**
 * Routing decisions, observed on the wire and at two real relays.
 *
 * # Why this is not a Go test
 *
 * internal/ruleset covers what the rules MEAN and internal/smtpsrv covers when
 * they fire. Neither can show a decision arriving at two different next hops,
 * because both substitute the delivery side. And nothing anywhere covers
 * swapping the ruleset on a live process and watching the very next transaction
 * route differently — the property that makes `ratelimit`, `auth` and the rules
 * "read live" rather than "needs a restart".
 *
 * This file automates TP-03 steps 3-9.
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import {
    baseline,
    haveBinary,
    relayEverythingTo,
    ruleset,
    startGateway,
    startSink,
    type Gateway,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

/** The ruleset TP-03 uses: two destinations, two refusals, a default. */
function tp03(): string {
    return ruleset({
        policy: [
            "    - name: reject-blocked-domain\n" +
                "      priority: 100\n" +
                "      match: {field: rcpt.domain, op: eq, value: blocked.test}\n" +
                '      then: [{action: reject, code: 550, message: "5.7.1 not accepted"}]\n',
            "    - name: reject-large-from-partner\n" +
                "      priority: 200\n" +
                "      match:\n" +
                "          all:\n" +
                "              - {field: mail.from_domain, op: eq, value: partner.test}\n" +
                "              - {field: msg.size, op: gt, value: 1000}\n" +
                '      then: [{action: reject, code: 552, message: "5.3.4 too large from you"}]\n',
        ],
        routes: [
            "    - name: to-group-a\n" +
                "      priority: 100\n" +
                "      match: {field: rcpt.domain, op: eq, value: a.test}\n" +
                "      then: [{action: relay, relay: GroupA}]\n",
            "    - name: to-group-b\n" +
                "      priority: 200\n" +
                "      match: {field: rcpt.domain, op: eq, value: b.test}\n" +
                "      then: [{action: relay, relay: GroupB}]\n",
        ],
        default: '{action: tempfail, code: 451, message: "4.3.0 No route found"}',
    });
}

describe.skipIf(!enabled)("per-recipient routing", () => {
    let gw: Gateway;
    let a: Sink;
    let b: Sink;

    beforeAll(async () => {
        a = await startSink();
        b = await startSink();
        gw = await startGateway({
            bundle: baseline({
                routing: tp03(),
                relays: { ...a.relays("GroupA"), ...b.relays("GroupB") },
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        a?.stop();
        b?.stop();
    });

    test("a recipient-stage refusal lands on its own RCPT TO line", async () => {
        const c = await gw.smtp();
        await c.ehlo("tp03.test");
        expect((await c.mail("ok@example.test")).code).toBe(250);

        // The rule reads only rcpt.domain, so its stage is RCPT and the sender
        // finds out on the line where it named the address — not after DATA.
        const reply = await c.rcpt("user@blocked.test");
        expect(reply.code).toBe(550);
        expect(reply.raw).toContain("5.7.1 not accepted");
        await c.quit();
    });

    test("one refused recipient does not sink the others", async () => {
        a.reset();
        const c = await gw.smtp();
        const marker = "mixed-" + Date.now();
        const { rcptReplies, queued } = await c.sendMail({
            from: "ok@example.test",
            to: ["user@blocked.test", "user@a.test"],
            subject: marker,
        });
        await c.quit();

        expect(rcptReplies[0].reply.code).toBe(550);
        expect(rcptReplies[1].reply.code).toBe(250);
        expect(queued.code).toBe(250);

        const msg = await a.waitForMessage((m) => m.data.includes(marker));
        expect(msg.rcpts).toEqual(["user@a.test"]);
    });

    test("a message to two groups is split, and both copies arrive", async () => {
        a.reset();
        b.reset();

        const c = await gw.smtp();
        const marker = "split-" + Date.now();
        const { queued } = await c.sendMail({
            from: "ok@example.test",
            to: ["user@a.test", "other@b.test"],
            subject: marker,
        });
        await c.quit();

        // One 250 for the message, whatever it splits into.
        expect(queued.code).toBe(250);

        const [inA, inB] = await Promise.all([
            a.waitForMessage((m) => m.data.includes(marker)),
            b.waitForMessage((m) => m.data.includes(marker)),
        ]);

        // This is what a first-recipient-wins relay gets wrong: Haraka routed
        // the whole message by rcpt_to[0], so one of these would have gone to
        // the wrong destination entirely.
        expect(inA.rcpts).toEqual(["user@a.test"]);
        expect(inB.rcpts).toEqual(["other@b.test"]);

        // Two envelopes over one transaction, sharing a body: the copies are
        // byte-identical apart from the envelope they were delivered under.
        expect(inA.headers["subject"]).toEqual(inB.headers["subject"]);
    });

    test("a data-stage rule cannot fire before the body exists", async () => {
        const c = await gw.smtp();
        await c.ehlo("tp03.test");
        // The rule ANDs mail.from_domain with msg.size, so its stage is the
        // later of the two. Everything up to and including RCPT is accepted
        // because the size is not knowable yet.
        expect((await c.mail("bulk@partner.test")).code).toBe(250);
        expect((await c.rcpt("user@a.test")).code).toBe(250);
        expect((await c.cmd("DATA")).code).toBe(354);

        // Wide, not long: a single 2000-character line would be refused by
        // max.line_length first (500 5.5.2, permanent since M10) and the test
        // would pass for the wrong reason.
        const body = Array.from({ length: 40 }, (_, i) => `line ${i} ` + "x".repeat(50)).join(
            "\r\n",
        );
        const reply = await c.dataBody(`Subject: big\r\n\r\n${body}\r\n`);
        expect(reply.code).toBe(552);
        expect(reply.raw).toContain("5.3.4 too large from you");
        await c.quit();
    });

    test("and a small message from the same sender is accepted", async () => {
        a.reset();
        const c = await gw.smtp();
        const marker = "small-" + Date.now();
        const { queued } = await c.sendMail({
            from: "bulk@partner.test",
            to: "user@a.test",
            subject: marker,
            body: "short",
        });
        await c.quit();

        // The rule's two conditions are ANDed, so the sender alone is not enough.
        expect(queued.code).toBe(250);
        await a.waitForMessage((m) => m.data.includes(marker));
    });

    test("an unrouted recipient gets the default action, at DATA", async () => {
        const c = await gw.smtp();
        await c.ehlo("tp03.test");
        await c.mail("ok@example.test");
        // Accepted at RCPT: default_action applies at DATA only, which preserves
        // the timing Haraka's hook_get_mx had.
        expect((await c.rcpt("user@nowhere.test")).code).toBe(250);
        await c.cmd("DATA");

        const reply = await c.dataBody("Subject: nowhere\r\n\r\nbody\r\n");
        // 4xx, deliberately: a 5xx would turn a forgotten route rule into
        // permanently rejected mail.
        expect(reply.code).toBe(451);
        expect(reply.raw).toContain("No route found");
        await c.quit();
    });
});

describe.skipIf(!enabled)("swapping the rules on a live gateway", () => {
    let gw: Gateway;
    let a: Sink;
    let b: Sink;

    beforeAll(async () => {
        a = await startSink();
        b = await startSink();
        gw = await startGateway({
            bundle: baseline({
                routing: relayEverythingTo("GroupA"),
                relays: { ...a.relays("GroupA"), ...b.relays("GroupB") },
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        a?.stop();
        b?.stop();
    });

    test("a new ruleset takes effect on the next transaction, with no restart", async () => {
        const first = await gw.smtp();
        const m1 = "before-" + Date.now();
        await first.sendMail({ from: "s@example.test", to: "r@x.test", subject: m1 });
        await first.quit();
        await a.waitForMessage((m) => m.data.includes(m1));

        const applied = await gw.ctl.applyBundle(
            baseline({
                routing: relayEverythingTo("GroupB"),
                relays: { ...a.relays("GroupA"), ...b.relays("GroupB") },
            }),
        );
        // The rules and the allowlist are the only two things swapped in place.
        // Anything on this list would mean the change had NOT taken effect.
        expect(applied.restart_required).toEqual([]);

        const second = await gw.smtp();
        const m2 = "after-" + Date.now();
        await second.sendMail({ from: "s@example.test", to: "r@x.test", subject: m2 });
        await second.quit();

        await b.waitForMessage((m) => m.data.includes(m2));
        // And nothing new reached the old destination.
        expect(a.messages.some((m) => m.data.includes(m2))).toBe(false);
    });

    test("an in-flight transaction finishes under the rules it started with", async () => {
        a.reset();
        b.reset();

        // Back to GroupA, then open a session and get as far as RCPT.
        await gw.ctl.applyBundle(
            baseline({
                routing: relayEverythingTo("GroupA"),
                relays: { ...a.relays("GroupA"), ...b.relays("GroupB") },
            }),
        );

        const c = await gw.smtp();
        const marker = "mid-session-" + Date.now();
        await c.ehlo("swap.test");
        await c.mail("s@example.test");
        await c.rcpt("r@x.test");

        // Swap underneath it. A session reads each atomic pointer once, so an
        // apply never takes effect halfway through a transaction — the property
        // gateway.go's comment claims and that nothing tested.
        await gw.ctl.applyBundle(
            baseline({
                routing: relayEverythingTo("GroupB"),
                relays: { ...a.relays("GroupA"), ...b.relays("GroupB") },
            }),
        );

        await c.cmd("DATA");
        expect((await c.dataBody(`Subject: ${marker}\r\n\r\nbody\r\n`)).code).toBe(250);
        await c.quit();

        const msg = await a.waitForMessage((m) => m.data.includes(marker));
        expect(msg.rcpts).toEqual(["r@x.test"]);
    });
});

describe.skipIf(!enabled)("the allowlist", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({ bundle: baseline({ relays: sink.relays() }) });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("swaps in place, and refuses before the banner", async () => {
        // An empty list with allow_all off denies everything — the fail-closed
        // zero value, and the cleanest way to assert the refusal without the
        // test depending on what address the gateway sees it arrive from.
        const applied = await gw.ctl.applyBundle(
            baseline({
                relays: sink.relays(),
                allowlist: { allowed: ["10.255.255.1"], allow_all: false },
            }),
        );
        expect(applied.restart_required).toEqual([]);

        const addr = await gw.smtpAddr();
        const [host, port] = addr.split(":");
        const { SmtpClient } = await import("../harness/smtp.ts");
        const c = await SmtpClient.open(host, Number(port));

        // The refusal replaces the greeting: go-smtp calls NewSession at EHLO,
        // so denying before the banner has to happen in front of the server.
        const first = await c.greeting();
        expect(first.code).toBe(550);
        expect(first.raw).toMatch(/denied/i);
        c.close();
    });

    test("and letting the peer back in needs no restart either", async () => {
        await gw.ctl.applyBundle(
            baseline({ relays: sink.relays(), allowlist: { allowed: [], allow_all: true } }),
        );
        const c = await gw.smtp();
        expect((await c.ehlo("back-in.test")).code).toBe(250);
        await c.quit();
    });

    test("a malformed allowlist is refused, and the gateway keeps the old one", async () => {
        const reason = await gw.ctl.applyExpectingFailure(
            baseline({
                relays: sink.relays(),
                allowlist: { allowed: ["not-an-address"], allow_all: false },
            }),
        );
        expect(reason).toContain("not-an-address");

        // Fail-closed here means the previous allowlist stays in force, not
        // that the gateway starts denying everything.
        const c = await gw.smtp();
        expect((await c.ehlo("still-allowed.test")).code).toBe(250);
        await c.quit();
    });
});
