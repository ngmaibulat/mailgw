/**
 * What survives a process death, and what honestly does not.
 *
 * # Why this is not a Go test
 *
 * Everything here is a claim about TWO processes over ONE data directory, and a
 * Go test cannot easily kill and re-exec itself. Boot-from-cache is asserted
 * in-process by internal/node, which proves the code path; it cannot prove that
 * SQLite, its WAL and the spool actually survive a SIGTERM and are readable by a
 * different process.
 *
 * The restart_required half is the more interesting one. That list is a promise
 * to an operator — "this change needs a restart" — and the only way to check the
 * promise is honest is to make the change, confirm on the wire that it has NOT
 * taken effect, restart, and confirm that it now has.
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import {
    baseline,
    haveBinary,
    startGateway,
    startSink,
    type Gateway,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

describe.skipIf(!enabled)("booting from the cache", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), hostname: "persisted.test" }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("the configuration comes back with no console in sight", async () => {
        const before = await gw.ctl.status();
        const beforeConfig = await gw.ctl.config();
        expect(before.serving).toBe(true);

        await gw.restart();

        // Waited for rather than read once: the control API answers as soon as
        // it binds, and Run boots from the cache on its own goroutine.
        const after = await gw.waitUntilServing();
        // There is no other configuration source. The bundle came out of the
        // local SQLite cache, which is the entire reason that cache exists —
        // booting with Central Management unreachable, or in this case absent.
        expect(after.serving).toBe(true);
        expect(after.applied_version_id).toBe(before.applied_version_id);
        expect((await gw.ctl.config()).sha256).toBe(beforeConfig.sha256);
    }, 60_000);

    test("the identity and the claim code survive", async () => {
        const st = await gw.ctl.status();
        // Losing these is what makes a node a stranger to its console and
        // forces re-approval, so they are the thing deploy/README tells an
        // operator to back up.
        expect(st.fingerprint).toMatch(/^[0-9a-f]{64}$/);
    });

    test("and it is serving on a new port, which the status reports", async () => {
        const st = await gw.ctl.status();
        expect(st.listeners).toHaveLength(1);

        const c = await gw.smtp();
        // The banner names the configured hostname, which came back from cache.
        const greeting = c.getTranscript();
        expect(greeting).toContain("persisted.test");
        await c.quit();
    }, 30_000);
});

describe.skipIf(!enabled)("restart_required is an honest list", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), hostname: "before.test" }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("a hostname change is named, and has NOT taken effect", async () => {
        const applied = await gw.ctl.applyBundle(
            baseline({ relays: sink.relays(), hostname: "after.test" }),
        );

        // smtpsrv.Backend.Cfg is captured at bring-up and read live at session
        // time, which is exactly why `hostname` is on the list.
        expect(applied.restart_required).toContain("hostname");

        const c = await gw.smtp();
        const banner = c.getTranscript();
        // The promise is that this has not changed yet. A list that named a key
        // the gateway had in fact hot-swapped would be worse than no list.
        expect(banner).toContain("before.test");
        expect(banner).not.toContain("after.test");
        await c.quit();
    }, 30_000);

    test("and it does take effect after a restart", async () => {
        await gw.restart();
        await gw.waitUntilServing();

        const c = await gw.smtp();
        expect(c.getTranscript()).toContain("after.test");
        await c.quit();

        // The list is about the LAST apply, so a restart that changed nothing
        // reports nothing.
        expect((await gw.ctl.status()).restart_required).toEqual([]);
    }, 60_000);

    test("changing only the rules names nothing at all", async () => {
        const applied = await gw.ctl.applyBundle(
            baseline({
                relays: sink.relays(),
                hostname: "after.test",
                routing:
                    "version: 1\nroutes:\n" +
                    "    - name: Renamed\n" +
                    "      match: {always: true}\n" +
                    "      then: [{action: relay, relay: Outbound}]\n",
            }),
        );
        // Rules and the allowlist are the two things swapped in place.
        expect(applied.restart_required).toEqual([]);
    }, 30_000);
});

describe.skipIf(!enabled)("a failed apply does not become what the next boot runs", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({
            bundle: baseline({ relays: sink.relays(), hostname: "good.test" }),
        });
    });

    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("the rejected bundle is not booted after a restart", async () => {
        const good = await gw.ctl.config();

        await gw.ctl.applyExpectingFailure(
            baseline({
                relays: sink.relays(),
                hostname: "bad.test",
                routing:
                    "version: 1\nroutes:\n" +
                    "    - name: Broken\n" +
                    "      match: {always: true}\n" +
                    "      then: [{action: relay, relay: NoSuchGroup}]\n",
            }),
        );

        await gw.restart();
        await gw.waitUntilServing();

        // desired_version_id is recorded only AFTER a successful apply here,
        // which is where the injection path deliberately differs from the pull
        // loop: there it is the console's intent and is authoritative even for a
        // bundle that failed, because an operator has to see what they asked
        // for. Here the caller already has the error, and leaving the pointer on
        // a bundle that can never apply would make every restart boot a failure
        // and fall back.
        const after = await gw.ctl.config();
        expect(after.sha256).toBe(good.sha256);

        const st = await gw.ctl.status();
        expect(st.serving).toBe(true);
        expect(st.apply_error).toBe("");

        const c = await gw.smtp();
        expect(c.getTranscript()).toContain("good.test");
        await c.quit();
    }, 90_000);
});

describe.skipIf(!enabled)("reset", () => {
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

    test("takes effect on the next boot, which is what it said", async () => {
        await gw.ctl.reset();
        // It cannot stop this process serving, and says so rather than letting a
        // test believe otherwise.
        expect((await gw.ctl.status()).serving).toBe(true);

        await gw.restart();

        // Waited for by its LOG rather than by polling status: "not serving" is
        // instantly true after a restart whether or not the boot has run, so
        // polling for it would pass before the gateway had decided anything.
        await gw.waitForLog(
            (r) => typeof r.msg === "string" && r.msg.includes("no cached configuration"),
        );

        // Now it is the fresh-data-volume state: no configuration, not serving,
        // waiting to be provisioned.
        const st = await gw.ctl.status();
        expect(st.serving).toBe(false);
        expect(st.listeners).toEqual([]);
        await expect(gw.ctl.config()).rejects.toMatchObject({ status: 404 });
    }, 60_000);

    test("and the node can be configured again afterwards", async () => {
        await gw.ctl.applyBundle(baseline({ relays: sink.relays() }));
        const c = await gw.smtp();
        expect((await c.ehlo("reconfigured.test")).code).toBe(250);
        await c.quit();
    }, 30_000);
});
