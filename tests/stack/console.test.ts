/**
 * Deploy and rollback, driven through the console's own forms.
 *
 * # Why this is not a Tier-B test
 *
 * Tier B injects configuration straight into applyCached, which is the right
 * thing for a matrix of gateway behaviours and the wrong thing for this: the
 * whole subject here is webui-fastify's deploy machinery — composing a bundle
 * from profiles, minting an immutable ConfigVersion, notifying the gateway over
 * the WebSocket, and repointing at an older version on rollback. None of that
 * exists in the gateway, and nothing else in the repository exercises it.
 *
 * This is the one Tier-A file that MUTATES configuration. Every other file in
 * tests/stack is read-only with respect to it, which is how the shared gateway
 * is serialised: not a mutex, but one writer by construction.
 *
 * Automates TP-09.
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import { Testctl } from "../harness/testctl.ts";
import {
    ALLOWLIST,
    PROFILE_ALLOWLIST,
    PROFILE_RULESET,
    PROFILE_SERVER,
    RELAY_GROUP,
    RULESET,
    SERVER,
    TESTCTL_URL,
} from "./baseline.ts";
import { haveTestctl, sharedConsole, type Console } from "./console.ts";
import { skipTierA, waitForGatewayReady } from "./ready.ts";

const enabled =
    (await haveTestctl()) ||
    !skipTierA(
        "tests/stack/console.test.ts",
        `no control API at ${TESTCTL_URL}`,
        "pnpm build:mailgw-go:test && pnpm stack:test",
    );

const ctl = new Testctl(TESTCTL_URL);

/** A ruleset that differs from the baseline only by a rule name. */
const RENAMED = RULESET.replace("name: Default", "name: Renamed-By-The-Suite");

/** A ruleset the gateway's compiler must refuse. */
const BROKEN = `version: 1
routes:
    - name: Broken
      match: {always: true}
      then: [{action: relay, relay: NoSuchGroupAnywhere}]
`;

let con: Console;
let gatewayId = 0;
let rulesetId = 0;

/**
 * Converge on the baseline before asserting anything.
 *
 * At ENTRY rather than only at exit: an afterAll that never ran — because a
 * predecessor crashed — cannot be relied on, and that is precisely the case
 * that matters. Re-running the idempotent provisioning path costs a second.
 */
async function converge(timeoutMs = 90_000): Promise<void> {
    await con.updateProfile(rulesetId, "ruleset", PROFILE_RULESET, RULESET);
    await con.deploy(gatewayId, "converge");

    // Deliberately NOT waitForAppliedVersion: the baseline may already be what
    // is running, and the console repoints rather than minting on an unchanged
    // configuration (the test below asserts exactly that), so "wait for the id
    // to change" would hang for the whole timeout on the common path. What
    // converging means is a state, not a transition — the baseline ruleset is
    // applied, cleanly, and SMTP is bound.
    const deadline = Date.now() + timeoutMs;
    for (;;) {
        const st = await ctl.status().catch(() => null);
        // apply_error is the discriminator that matters here: it is what
        // distinguishes "converged" from "still refusing the BROKEN bundle
        // while serving the old good one", which is the state this is called
        // to recover from.
        if (st && st.applied_version_id > 0 && st.apply_error === "" && st.listeners.length > 0) {
            const cfg = await ctl.config().catch(() => null);
            const routing = (cfg?.bundle as { routing?: string } | undefined)?.routing ?? "";
            if (routing.includes("name: Default")) return;
        }
        if (Date.now() > deadline) {
            throw new Error(
                `never converged on the baseline: applied=${st?.applied_version_id} ` +
                    `apply_error=${st?.apply_error} last_error=${st?.last_error} ` +
                    `listeners=${JSON.stringify(st?.listeners)}`,
            );
        }
        await Bun.sleep(500);
    }
}

/**
 * Wait for the gateway to report a NEW applied version.
 *
 * `previous` is required, not optional. It was optional, `converge()` called it
 * with no argument, and `v !== undefined` is true of every version — so it
 * returned on whatever was already applied and waited for nothing at all.
 */
async function waitForAppliedVersion(previous: number, timeoutMs = 45_000): Promise<number> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
        const st = await ctl.status().catch(() => null);
        const v = st?.applied_version_id ?? 0;
        if (v > 0 && v !== previous) return v;
        if (Date.now() > deadline) {
            throw new Error(
                `the gateway never applied a new version (still ${v}, apply_error=${st?.apply_error})`,
            );
        }
        await Bun.sleep(500);
    }
}

describe.skipIf(!enabled)("deploying from the console", () => {
    beforeAll(async () => {
        // The gateway has to be past its first poll before any of this means
        // anything — see tests/stack/ready.ts. `pnpm provision` waits for the
        // same condition, so on the ordinary path this returns immediately.
        await waitForGatewayReady();

        con = await sharedConsole();

        const groupId = await con.ensureRelayGroup(RELAY_GROUP);
        rulesetId = await con.ensureProfile(PROFILE_RULESET, "ruleset", RULESET);
        const allowlistId = await con.ensureProfile(PROFILE_ALLOWLIST, "allowlist", ALLOWLIST);
        const serverId = await con.ensureProfile(PROFILE_SERVER, "server", SERVER);

        gatewayId = await con.waitForGateway();
        await con.approve(gatewayId);
        await con.assign(gatewayId, {
            ruleset: rulesetId,
            allowlist: allowlistId,
            server: serverId,
            relayGroups: [groupId],
        });
        await converge();
    }, 240_000);

    afterAll(async () => {
        // Best effort. The entry convergence above is what actually protects
        // the next file.
        try {
            await converge();
        } catch {
            /* the next run converges anyway */
        }
    }, 120_000);

    test("a changed profile reaches the gateway in seconds, not on the poll", async () => {
        const before = (await ctl.status()).applied_version_id;

        await con.updateProfile(rulesetId, "ruleset", PROFILE_RULESET, RENAMED);
        await con.deploy(gatewayId, "renamed");

        // The WebSocket makes a deploy land in milliseconds; the 15s poll is
        // the backstop. 45s is generous for either.
        const after = await waitForAppliedVersion(before);
        expect(after).not.toBe(before);
        expect(after).toBeGreaterThan(0);

        const { bundle } = (await ctl.config()) as { bundle: { routing: string } };
        expect(bundle.routing).toContain("Renamed-By-The-Suite");

        const st = await ctl.status();
        expect(st.serving).toBe(true);
        expect(st.apply_error).toBe("");
    }, 120_000);

    test("redeploying an unchanged configuration does not mint a new version", async () => {
        const before = (await ctl.status()).applied_version_id;
        await con.deploy(gatewayId, "unchanged");
        await Bun.sleep(3000);

        // The console compares a depth-sorted stable stringify and repoints
        // rather than piling up rows. Without this a nightly redeploy would
        // grow the version table for ever.
        expect((await ctl.status()).applied_version_id).toBe(before);
    }, 60_000);

    test("a configuration the gateway refuses leaves it serving the old one", async () => {
        const good = await ctl.config();

        await con.updateProfile(rulesetId, "ruleset", PROFILE_RULESET, BROKEN);
        await con.deploy(gatewayId, "broken");

        // The console does shape checks only — the DSL is compiled by Go — so
        // this deploy SUCCEEDS on the console and fails on the gateway, which
        // reports the reason back. That split is the design.
        const deadline = Date.now() + 45_000;
        let st = await ctl.status();
        while (Date.now() < deadline && !st.apply_error) {
            await Bun.sleep(500);
            st = await ctl.status();
        }

        expect(st.apply_error).toContain("NoSuchGroupAnywhere");
        // The load-bearing half: it is still serving, on the last good bundle.
        expect(st.serving).toBe(true);
        expect((await ctl.config()).sha256).toBe(good.sha256);
    }, 120_000);

    test("rollback restores a byte-identical configuration", async () => {
        // Recover from the broken deploy, and remember what good looks like.
        await converge();
        const good = await ctl.config();

        await con.updateProfile(rulesetId, "ruleset", PROFILE_RULESET, RENAMED);
        await con.deploy(gatewayId, "before-rollback");
        const changed = await waitForAppliedVersion(good.version_id);
        expect(changed).not.toBe(good.version_id);

        const res = await con.rollback(gatewayId, good.version_id);
        expect(res.status).toBeLessThan(400);

        await waitForAppliedVersion(changed);
        const back = await ctl.config();

        // Rollback repoints at an older row rather than minting a new bundle,
        // so what runs afterwards is byte-identical — not merely equivalent.
        expect(back.sha256).toBe(good.sha256);
        expect(back.version_id).toBe(good.version_id);
    }, 180_000);
});
