/**
 * The control API, against the real binary.
 *
 * # Why this is not a Go test
 *
 * internal/testctl/server_test.go drives the routing table against a fake
 * Control, and internal/node/control_test.go drives the real methods in
 * process. Neither runs the two together over a socket, and neither exercises
 * the binary at all — its flags, its exit codes, or the stdout line a harness
 * depends on.
 *
 * This file is also the wire-shape contract: tests/harness/testctl.ts
 * hand-mirrors the Go structs, and every assertion below fails if they drift.
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import {
    baseline,
    binaryPath,
    Gateway,
    haveBinary,
    relayEverythingTo,
    ruleset,
    startGateway,
    startSink,
    TestctlError,
    type Sink,
} from "../harness/index.ts";

const enabled = await haveBinary();

describe.skipIf(!enabled)("the binary's command line", () => {
    test("-testctl is required and its absence is a usage error", async () => {
        const proc = Bun.spawn([await binaryPath(), "-data", "/tmp/never-created"], {
            stdout: "pipe",
            stderr: "pipe",
        });
        const [code, stderr] = await Promise.all([
            proc.exited,
            new Response(proc.stderr).text(),
        ]);

        // 2, not 1: a script has to be able to tell "you typed it wrong" from
        // "it failed to start".
        expect(code).toBe(2);
        expect(stderr).toContain("-testctl is required");
    });

    test('-admin "" is fatal, and says why', async () => {
        const proc = Bun.spawn(
            [await binaryPath(), "-testctl", "127.0.0.1:0", "-admin", "", "-data", "/tmp/never-created"],
            { stdout: "pipe", stderr: "pipe" },
        );
        const [code, stderr] = await Promise.all([
            proc.exited,
            new Response(proc.stderr).text(),
        ]);

        // A managed node with no wizard cannot be provisioned at all, so the
        // binary refuses rather than starting something useless. The usage text
        // used to claim "" disabled the UI; it never did.
        expect(code).toBe(2);
        expect(stderr).toContain("admin");
    });

    test("-version identifies the build as the test one", async () => {
        const proc = Bun.spawn([await binaryPath(), "-version"], { stdout: "pipe", stderr: "pipe" });
        const [code, stdout] = await Promise.all([proc.exited, new Response(proc.stdout).text()]);
        expect(code).toBe(0);
        expect(stdout).toContain("mailgw-go-test");
    });
});

describe.skipIf(!enabled)("an unconfigured gateway", () => {
    let gw: Gateway;

    beforeAll(async () => {
        gw = await startGateway();
    });
    afterAll(async () => {
        await gw?.stop();
    });

    test("says what it is, loudly", async () => {
        const st = await gw.ctl.status();
        // Nothing may mistake this for a shipped node — not a person reading
        // the status, and not a console reading the fleet view.
        expect(st.build).toBe("test-only");
        expect(gw.logs()).toContain("THIS IS THE TEST BUILD");
    });

    test("is not serving, and holds no configuration", async () => {
        const st = await gw.ctl.status();
        expect(st.serving).toBe(false);
        expect(st.listeners).toEqual([]);
        expect(st.provisioned).toBe(false);
        expect(st.applied_version).toBeNull();

        // 404 rather than an empty object: "no configuration" and "an empty
        // configuration" are different states and a test must not confuse them.
        await expect(gw.ctl.config()).rejects.toMatchObject({ status: 404 });
    });

    test("has no spool, and says so with a 409", async () => {
        // Not 500: the request was fine, the gateway just has no queue yet.
        await expect(gw.ctl.queue()).rejects.toMatchObject({ status: 409 });
    });

    test("reports the bound admin address, not the requested one", async () => {
        const st = await gw.ctl.status();
        expect(st.admin_addr).not.toBe("");
        expect(st.admin_addr.endsWith(":0")).toBe(false);

        // Reported is not enough: it has to answer. /healthz is open even here,
        // before any configuration and whatever the metrics token says.
        const res = await fetch(`http://${st.admin_addr}/healthz`);
        expect(res.status).toBe(200);
    });

    test("mints a claim code and logs it while unclaimed", async () => {
        const warn = gw
            .logLines()
            .find((r) => typeof r.msg === "string" && r.msg.includes("unclaimed"));
        expect(warn).toBeDefined();
        expect(String(warn?.code ?? "")).toMatch(/^[0-9A-Z-]{10,}$/);
    });
});

describe.skipIf(!enabled)("applying a configuration", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway();
    });
    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("a bundle asking for :0 binds a real port and reports it", async () => {
        const applied = await gw.ctl.applyBundle(baseline({ relays: sink.relays() }));

        // Negative, so an injected id can never collide with the console's
        // positive autoincrement.
        expect(applied.version_id).toBeLessThan(0);
        // [] and not null, or every caller has to special-case it.
        expect(applied.restart_required).toEqual([]);

        const st = await gw.ctl.status();
        expect(st.serving).toBe(true);
        expect(st.listeners).toHaveLength(1);
        expect(st.listeners[0].endsWith(":0")).toBe(false);
        expect(st.spool_dir).toContain(gw.dataDir);
    });

    test("the reported address actually answers SMTP", async () => {
        const c = await gw.smtp();
        const ehlo = await c.ehlo("control.test");
        expect(ehlo.code).toBe(250);
        await c.quit();
    });

    test("identical bytes are one version, re-injected", async () => {
        const bundle = baseline({ relays: sink.relays() });
        const first = await gw.ctl.applyBundle(bundle);
        const second = await gw.ctl.applyBundle(bundle);

        // A counter would give one configuration two ids and make "what is this
        // node running?" ambiguous after a re-apply.
        expect(second.version_id).toBe(first.version_id);
        expect(second.sha256).toBe(first.sha256);
    });

    test("the applied bundle comes back unredacted", async () => {
        await gw.ctl.applyBundle(
            baseline({
                relays: sink.relays("Outbound", { auth_user: "u", auth_pass: "hunter2" }),
                logging: { url_conn: "http://logs.test/c", api_key: "secret-key" },
            }),
        );

        const cached = await gw.ctl.config();
        const raw = JSON.stringify(cached.bundle);
        // A test asserting on a credential it just injected cannot do that
        // against "[redacted]". `mailgw-go config show` keeps redacting.
        expect(raw).toContain("hunter2");
        expect(raw).toContain("secret-key");
        expect(raw).not.toContain("[redacted]");
    });
});

describe.skipIf(!enabled)("refusing a bad configuration", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({});
        await gw.ctl.applyBundle(baseline({ relays: sink.relays() }));
    });
    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("a rule naming an unknown relay group is refused, with the reason", async () => {
        const reason = await gw.ctl.applyExpectingFailure(
            baseline({
                relays: sink.relays(),
                routing: relayEverythingTo("NoSuchGroup"),
            }),
        );
        expect(reason).toContain("NoSuchGroup");
    });

    test("a misspelt field is refused at load time, not silently never matched", async () => {
        const reason = await gw.ctl.applyExpectingFailure(
            baseline({
                relays: sink.relays(),
                routing: ruleset({
                    routes: [
                        "    - name: typo\n" +
                            "      match: {field: rcpt.doman, op: eq, value: x.test}\n" +
                            "      then: [{action: relay, relay: Outbound}]\n",
                    ],
                }),
            }),
        );
        // The registry exists precisely so this is an error rather than a rule
        // that quietly never fires.
        expect(reason).toContain("rcpt.doman");
    });

    test("junk is refused before it reaches the bundle parser", async () => {
        const res = await gw.ctl.raw("POST", "/testctl/config", "not json at all");
        expect(res.status).toBe(400);
        expect(await res.text()).toContain("JSON");
    });

    test("and the gateway keeps serving the last good configuration throughout", async () => {
        // The load-bearing assertion of this whole file: fail-closed means the
        // previous configuration stays in force, not that the gateway stops.
        const st = await gw.ctl.status();
        expect(st.serving).toBe(true);

        const c = await gw.smtp();
        expect((await c.ehlo("still-here.test")).code).toBe(250);
        await c.quit();
    });
});

describe.skipIf(!enabled)("the routing table", () => {
    let gw: Gateway;

    beforeAll(async () => {
        gw = await startGateway();
    });
    afterAll(async () => {
        await gw?.stop();
    });

    test("an unknown path is 404 and a wrong method is 405", async () => {
        expect((await gw.ctl.raw("GET", "/testctl/nope")).status).toBe(404);
        expect((await gw.ctl.raw("GET", "/testctl/enroll")).status).toBe(405);
    });

    test("enroll needs a console URL", async () => {
        await expect(gw.ctl.enroll({ central_url: "" })).rejects.toMatchObject({ status: 400 });
    });

    test("enroll reports an unreachable console as 502, not 400", async () => {
        // 127.0.0.1:1 refuses instantly. The distinction matters: a test that
        // cannot tell "my request was wrong" from "the console is down" spends
        // its time debugging the wrong end.
        const err = (await gw.ctl
            .enroll({ central_url: "http://127.0.0.1:1" })
            .catch((e) => e)) as TestctlError;
        expect(err).toBeInstanceOf(TestctlError);
        expect(err.status).toBe(502);
    });

    test("the three envelope verbs refuse an empty list", async () => {
        for (const path of ["release", "hold", "remove"]) {
            const res = await gw.ctl.raw("POST", `/testctl/queue/${path}`, "{}");
            // Unlike flush, where no uuids means "the whole ready queue".
            // "Release everything ever quarantined" is not a gesture anybody
            // wants to make by leaving a field out.
            expect(res.status).toBe(400);
        }
    });
});

describe.skipIf(!enabled)("reset", () => {
    let gw: Gateway;
    let sink: Sink;

    beforeAll(async () => {
        sink = await startSink();
        gw = await startGateway({});
        await gw.ctl.applyBundle(baseline({ relays: sink.relays() }));
    });
    afterAll(async () => {
        await gw?.stop();
        sink?.stop();
    });

    test("clears the cache but admits it cannot stop the gateway serving", async () => {
        const out = await gw.ctl.reset();
        expect(out.reset).toBe(true);
        // Listeners, the relay table and the spool are bound for the life of the
        // process. Claiming otherwise would hand a test a false pass.
        expect(out.restart_required).toBe(true);

        expect((await gw.ctl.status()).serving).toBe(true);
        await expect(gw.ctl.config()).rejects.toMatchObject({ status: 404 });
    });
});
