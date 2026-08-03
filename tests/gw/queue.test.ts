/**
 * Deferral, retry, quarantine and the operator verbs — against a relay that can
 * be broken and repaired.
 *
 * # Why this is not a Go test
 *
 * internal/queue's tests act on a spool built by hand and internal/deliver's
 * drive the client directly. What is only true out of process is that these
 * verbs act on a spool a RUNNING gateway owns, that the scheduler notices a
 * released envelope without a restart, and that a crash-and-restart recovers
 * what was in flight.
 *
 * This automates TP-06 steps 2-8 and 10-14, which currently cost about forty
 * minutes of somebody's time.
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
    type QueueEntry,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

/** A short retry ladder, so the test asserts on behaviour rather than a clock. */
const FAST_OUTBOUND =
    "outbound:\n" +
    "    backoff: [1s, 2s, 4s]\n" +
    "    max_lifetime: 30s\n" +
    "    poll_interval: 1s\n";

/** Poll the control API until an envelope reaches a state. */
async function waitForEntry(
    gw: Gateway,
    pred: (e: QueueEntry) => boolean,
    timeoutMs = 30_000,
): Promise<QueueEntry> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
        const entries = await gw.ctl.queue();
        const hit = entries.find(pred);
        if (hit) return hit;
        if (Date.now() > deadline) {
            throw new Error(
                gw.decorate(
                    `no queue entry matched in ${timeoutMs}ms; queue = ${JSON.stringify(entries)}`,
                ),
            );
        }
        await Bun.sleep(100);
    }
}

/** Wait for the spool to empty, which is how "it delivered" reads. */
async function waitForEmpty(gw: Gateway, timeoutMs = 30_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
        const entries = await gw.ctl.queue();
        if (entries.length === 0) return;
        if (Date.now() > deadline) {
            throw new Error(gw.decorate(`the spool never drained: ${JSON.stringify(entries)}`));
        }
        await Bun.sleep(100);
    }
}

describe.skipIf(!enabled)("a relay that is down", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), serverExtra: FAST_OUTBOUND }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("the message is still accepted — the relay is not the sender's problem", async () => {
        sink.down();

        const c = await gw.smtp();
        const { queued } = await c.sendMail({
            from: "s@example.test",
            to: "r@partner.test",
            subject: "deferral",
        });
        await c.quit();

        // This is the durability property: the message is on disk before the
        // client is told 250.
        expect(queued.code).toBe(250);
    }, 30_000);

    test("it appears in the queue with a reason and a next-attempt time", async () => {
        const entry = await waitForEntry(gw, (e) => e.attempts >= 1);

        expect(entry.queue).toBe("q");
        expect(entry.sender).toBe("s@example.test");
        expect(entry.rcpts).toEqual(["r@partner.test"]);
        expect(entry.relay_group).toBe("Outbound");
        // The reason has to name the failure. An MX resolution failure used to
        // leave this blank on a first attempt, which made mailq useless in
        // exactly the case it exists for.
        expect(entry.last_error).toBeTruthy();
        expect(entry.due).toBeTruthy();
    }, 60_000);

    test("attempts keep climbing while it is down", async () => {
        const before = (await gw.ctl.queue())[0]?.attempts ?? 0;
        const after = await waitForEntry(gw, (e) => e.attempts > before);
        expect(after.attempts).toBeGreaterThan(before);
    }, 60_000);

    test("bringing the relay back, and flushing, delivers it now", async () => {
        sink.up();

        // flush is the operational move after an outage: without it a recovered
        // relay waits out the backoff, which on the shipped schedule is hours.
        const flushed = await gw.ctl.flush();
        expect(flushed).toBeGreaterThan(0);

        await sink.waitForMessage((m) => m.data.includes("deferral"));
        await waitForEmpty(gw);
    }, 60_000);
});

describe.skipIf(!enabled)("quarantine", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                serverExtra: FAST_OUTBOUND,
                // Two things about this ruleset are load-bearing, and TP-06's
                // own example gets the first one wrong.
                //
                // 1. The quarantine is a POLICY rule, not a route. As a route
                //    action it is a DROP: split() sees a decision that resolves
                //    to no relay group and reasons that a quarantined envelope
                //    still needs somewhere to go if it is ever released, so it
                //    discards instead. Only a policy quarantine sets the flag
                //    while a route rule still names a group.
                // 2. It matches a HEADER, which puts it at the data stage. A
                //    quarantine whose fields are all known by RCPT is decided
                //    before there is a message to hold, and is likewise a drop.
                routing: ruleset({
                    policy: [
                        "    - name: quarantine-suspicious\n" +
                            "      priority: 50\n" +
                            "      match: {field: header.subject, op: contains, value: HOLDME}\n" +
                            "      then: [{action: quarantine}]\n",
                    ],
                    routes: [
                        "    - name: Default\n" +
                            "      priority: 100\n" +
                            "      match: {always: true}\n" +
                            "      then: [{action: relay, relay: Outbound}]\n",
                    ],
                }),
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("a quarantine rule accepts the message and then holds it", async () => {
        const c = await gw.smtp();
        const { queued } = await c.sendMail({
            from: "x@suspicious.test",
            to: "r@partner.test",
            subject: "HOLDME please",
        });
        await c.quit();

        // 250: quarantine accepts. The sender is not told, which is the point.
        expect(queued.code).toBe(250);

        const entry = await waitForEntry(gw, (e) => e.queue === "quarantine");
        expect(entry.sender).toBe("x@suspicious.test");

        // And nothing delivers it, however long you wait.
        await sink.expectNothing(1500);
    }, 30_000);

    test("release puts it back, and the scheduler picks it up", async () => {
        const held = (await gw.ctl.queue()).find((e) => e.queue === "quarantine")!;
        expect(await gw.ctl.release([held.uuid])).toBe(1);

        // The real assertion is that it DELIVERS. quarantine/ names files
        // "<uuid>.json" and the ready queue needs "<due>.<uuid>.json", so a bare
        // rename would produce a name the scheduler skips: the envelope would
        // sit in q/, be counted as ready, and be claimed by nobody. Released
        // mail that silently never moves is worse than mail still visibly held.
        await sink.waitForMessage((m) => m.data.includes("HOLDME"));
        await waitForEmpty(gw);
    }, 30_000);

    test("hold is the reverse, and remove drops it", async () => {
        sink.down();

        const c = await gw.smtp();
        await c.sendMail({ from: "s@example.test", to: "r@partner.test", subject: "to-hold" });
        await c.quit();

        const queued = await waitForEntry(gw, (e) => e.queue === "q");
        expect(await gw.ctl.hold([queued.uuid])).toBe(1);

        const held = await waitForEntry(gw, (e) => e.queue === "quarantine");
        expect(held.uuid).toBe(queued.uuid);

        expect(await gw.ctl.remove([held.uuid])).toBe(1);
        expect(await gw.ctl.queue()).toEqual([]);

        sink.up();
    }, 30_000);

    test("an unknown uuid is reported rather than silently ignored", async () => {
        await expect(gw.ctl.release(["no-such-envelope"])).rejects.toMatchObject({
            status: 409,
        });
    });
});

describe.skipIf(!enabled)("expiry", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                serverExtra:
                    "outbound:\n" +
                    "    backoff: [1s]\n" +
                    // Short enough that the test does not sit on a clock, long
                    // enough that a slow CI runner still gets two attempts in.
                    "    max_lifetime: 5s\n" +
                    "    poll_interval: 1s\n" +
                    "dsn:\n" +
                    "    enabled: true\n" +
                    "    relay_group: Outbound\n",
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("a message past max_lifetime is buried, and its sender is told", async () => {
        sink.down();

        const c = await gw.smtp();
        await c.sendMail({ from: "s@example.test", to: "r@partner.test", subject: "expiring" });
        await c.quit();

        // Wait for it to reach dead/ — the envelope is given up on.
        const dead = await waitForEntry(gw, (e) => e.queue === "dead", 40_000);
        expect(dead.sender).toBe("s@example.test");

        // Now let the notification out. It is itself mail and needs a relay.
        sink.up();
        await gw.ctl.flush().catch(() => 0);

        const bounce = await sink.waitForMessage(
            (m) => m.data.includes("delivery-status"),
            40_000,
        );
        // The null sender is what stops two mail systems bouncing at each other
        // for ever.
        expect(bounce.from).toBe("");
        expect(bounce.rcpts).toEqual(["s@example.test"]);
        expect(bounce.headers["content-type"]?.[0]).toContain("report-type=delivery-status");
    }, 90_000);

    test("a buried envelope keeps the reason it was given up on", async () => {
        const dead = (await gw.ctl.queue()).find((e) => e.queue === "dead");
        expect(dead).toBeDefined();
        // dead/ is where an envelope goes to be explained, so the last error has
        // to survive with it: it is what the expiry notification quoted, and the
        // only evidence left once the body has been collected.
        expect(dead?.last_error ?? "").toBeTruthy();
        // And there is deliberately no way back out of dead/ — the message it
        // describes no longer exists, so release refuses it.
        await expect(gw.ctl.release([dead!.uuid])).rejects.toMatchObject({ status: 409 });
    }, 30_000);
});

describe.skipIf(!enabled)("across a restart", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({
                relays: sink.relays(),
                routing: relayEverythingTo("Outbound"),
                serverExtra: FAST_OUTBOUND,
            }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("queued mail survives, and the new process delivers it", async () => {
        sink.down();

        const c = await gw.smtp();
        const marker = "survives-restart-" + Date.now();
        await c.sendMail({ from: "s@example.test", to: "r@partner.test", subject: marker });
        await c.quit();
        await waitForEntry(gw, (e) => e.queue === "q");

        // A different process, over the same data directory. The configuration
        // comes back from the SQLite cache — no console is involved, and there
        // is no configuration source other than that cache.
        await gw.restart();

        // Waited for, not read once: the control API answers as soon as it
        // binds and Run boots from the cache on its own goroutine, so reading
        // the status straight away is a race that passes on a fast machine.
        const st = await gw.waitUntilServing();
        expect(st.serving).toBe(true);
        expect(st.listeners).toHaveLength(1);

        // The envelope is still there, written by the process that died.
        const entries = await gw.ctl.queue();
        expect(entries.length).toBeGreaterThan(0);

        sink.up();
        await gw.ctl.flush();
        await sink.waitForMessage((m) => m.data.includes(marker));
    }, 90_000);
});
