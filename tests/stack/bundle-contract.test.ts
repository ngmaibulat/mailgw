/**
 * The bundle the CONSOLE composes, read back from the gateway that applied it.
 *
 * # Why this exists
 *
 * `webui-fastify/src/central/bundle.ts` composes a document in TypeScript;
 * `mailgw-go/internal/config` parses it in Go. Each side has its own tests and
 * neither has ever seen the other's output. The shape mismatches bundle.ts
 * handles — relay-group name as the top-level key, `Relays.host` -> `exchange`,
 * port INT -> number — are exactly the kind of thing that drifts silently.
 *
 * This is the only assertion in the repository that the two agree, and it needs
 * the engineering build only to READ the applied bundle: the bundle itself came
 * from the console, through the ordinary signed pull, exactly as production's
 * does.
 */

import { beforeAll, describe, expect, test } from "bun:test";

import { Testctl } from "../harness/testctl.ts";
import { RELAY_GROUP, RELAY_HOST, RELAY_PORT, TESTCTL_URL } from "./baseline.ts";
import { haveTestctl, sharedConsole } from "./console.ts";
import { skipTierA, waitForGatewayReady } from "./ready.ts";

const enabled =
    (await haveTestctl()) ||
    !skipTierA(
        "tests/stack/bundle-contract.test.ts",
        `no control API at ${TESTCTL_URL}`,
        "pnpm build:mailgw-go:test && pnpm stack:test",
    );

const ctl = new Testctl(TESTCTL_URL);

describe.skipIf(!enabled)("the console's bundle, as the gateway sees it", () => {
    // The gateway's poll loop waits a jittered 15s before its first
    // /agent/status, and registration does not wake it — so a suite that starts
    // the moment `pnpm provision` returns is asserting against a gateway that is
    // still `pending` and holds nothing. This is that wait, and it is the same
    // one provisioning uses.
    beforeAll(async () => {
        await waitForGatewayReady();
    }, 120_000);

    test("it came from the console, not from an injection", async () => {
        const st = await ctl.status();
        expect(st.provisioned).toBe(true);
        expect(st.approval).toBe("approved");
        // Console version ids are a positive autoincrement; injected ones are
        // negative, derived from the bundle digest. A negative id here would
        // mean this suite was asserting on a configuration some other test
        // pushed in, which would prove nothing about the console at all.
        expect(st.applied_version_id).toBeGreaterThan(0);
    }, 45_000);

    test("every key the gateway needs is present and the right shape", async () => {
        const { bundle } = (await ctl.config()) as { bundle: any };

        expect(bundle.format).toBe(1);
        // server and routing travel as raw profile TEXT, not as parsed objects:
        // the console does shape checks only and the gateway owns the DSL.
        expect(typeof bundle.server).toBe("string");
        expect(typeof bundle.routing).toBe("string");

        // allow_all is passed through rather than stripped — without it an
        // allow-all gateway could not be expressed from the console at all.
        expect(bundle.allowlist).toBeDefined();
        expect(Array.isArray(bundle.allowlist.allowed)).toBe(true);

        // A managed gateway has no environment, so the logservice URLs travel
        // in the bundle.
        expect(bundle.logging).toBeDefined();
        expect(bundle.logging.url_conn).toContain("/api/connection");
        expect(bundle.logging.url_queue).toContain("/api/queue");
        expect(bundle.logging.url_delivery).toContain("/api/delivery");
        // 45s, not Bun's default 5s: Testctl.raw carries its own 30s abort, so
        // a shorter budget replaces the gateway's own message — which is the
        // single most useful thing the control API returns — with a generic
        // "timed out". The same applies to every test in this file.
    }, 45_000);

    test("the relay group survives bundle.ts's three shape changes", async () => {
        const { bundle } = (await ctl.config()) as { bundle: any };

        // 1. The relay-group NAME is the top-level key.
        const group = bundle.relays?.[RELAY_GROUP];
        expect(group).toBeDefined();
        expect(Array.isArray(group)).toBe(true);
        expect(group.length).toBeGreaterThan(0);

        // 2. Relays.host becomes `exchange`.
        expect(group[0].exchange).toBe(RELAY_HOST);

        // 3. The port is a NUMBER, from an INT column. Go accepts a string too,
        //    so a regression here would not fail the gateway — only this.
        expect(group[0].port).toBe(Number(RELAY_PORT));
    }, 45_000);

    test("optional keys are omitted when empty, so an unchanged config re-hashes", async () => {
        const { bundle } = (await ctl.config()) as { bundle: any };

        // The dev stack sets neither GATEWAY_METRICS_TOKEN nor a credential
        // set. Emitting `{"admin":{}}` instead of omitting the key would change
        // the digest and make every poll look like a new configuration.
        for (const key of ["admin", "auth"]) {
            if (key in bundle) {
                expect(Object.keys(bundle[key]).length).toBeGreaterThan(0);
            }
        }
    }, 45_000);

    test("the gateway's digest is the one the console recorded", async () => {
        const con = await sharedConsole();
        const id = await con.waitForGateway();
        const page = await con.gatewayPage(id);

        const { sha256 } = await ctl.config();
        expect(sha256).toMatch(/^[0-9a-f]{64}$/);

        // bundle_sha256 is an opaque change token that the gateway never
        // recomputes — Fastify re-serialises the bundle on the way out, so the
        // wire bytes are not the bytes that were hashed. Both sides must
        // therefore be carrying the SAME string, which is what this checks.
        expect(page).toContain(sha256.slice(0, 12));
    }, 60_000);
});
