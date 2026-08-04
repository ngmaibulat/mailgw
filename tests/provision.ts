/**
 * Provision the dev stack's gateway through Central Management.
 *
 * WHY THIS EXISTS
 *
 * The gateway used to be pinned to file mode in docker-compose.yaml: it read a
 * mounted config directory and relayed as soon as it started, so the SMTP e2e
 * suite could `docker compose up` and immediately send. File mode is gone —
 * Central Management is the only configuration source — so a freshly started
 * stack has a gateway that is deliberately NOT listening: unclaimed,
 * unregistered, unapproved and holding no configuration.
 *
 * This walks it the rest of the way, which is the same path an operator walks
 * on a real edge node:
 *
 *   1. create the console's first admin, and sign in
 *   2. create a relay group and a relay pointing at the mailhog sink
 *   3. create the three config profiles (ruleset, allowlist, server)
 *   4. wait for the gateway to register itself and approve its fingerprint
 *   5. assign the profiles and the relay group, then deploy
 *   6. wait until it has APPLIED that configuration and is answering on SMTP
 *
 * Step 6 is `tests/stack/ready.ts`, and it is shared with the Tier-A suites so
 * that "ready" has exactly one definition. It used to be a bare TCP connect to
 * the published SMTP port, which succeeds through docker-proxy whether or not
 * the gateway has bound anything — so this script reported success roughly a
 * millisecond after the deploy, and the tests raced a gateway that was still
 * pending and held no bundle. See that file.
 *
 * Every step is idempotent, so re-running against an already-provisioned stack
 * is a no-op and safe.
 *
 *   bun tests/provision.ts
 *
 * The console client and the profile texts live in tests/stack/, so the Tier-A
 * suites drive the console exactly as this does and assert against the same
 * baseline. This file is the script around them.
 */

import {
    ALLOWLIST,
    CONSOLE_URL,
    PROFILE_ALLOWLIST,
    PROFILE_RULESET,
    PROFILE_SERVER,
    RELAY_GROUP,
    RELAY_HOST,
    RELAY_PORT,
    RULESET,
    SERVER,
    TESTCTL_URL,
} from "./stack/baseline.ts";
import { Console, enrollGateway } from "./stack/console.ts";
import { describeReady, waitForGatewayReady } from "./stack/ready.ts";

async function main(): Promise<void> {
    console.log(`console:  ${CONSOLE_URL}`);
    const con = new Console();
    await con.signIn();
    console.log("signed in");

    const groupId = await con.ensureRelayGroup(RELAY_GROUP);
    console.log(`relay group ${RELAY_GROUP} (${groupId}) -> ${RELAY_HOST}:${RELAY_PORT}`);

    const ruleset = await con.ensureProfile(PROFILE_RULESET, "ruleset", RULESET);
    const allowlist = await con.ensureProfile(PROFILE_ALLOWLIST, "allowlist", ALLOWLIST);
    const server = await con.ensureProfile(PROFILE_SERVER, "server", SERVER);
    console.log(`profiles: ruleset=${ruleset} allowlist=${allowlist} server=${server}`);

    if (!(await enrollGateway())) {
        console.log(
            "no test control API at " +
                TESTCTL_URL +
                "; waiting for the gateway to be pointed at the console by hand " +
                "(open its wizard on :8080 — the claim code is in its log)",
        );
    }

    const gatewayId = await con.waitForGateway();
    await con.approve(gatewayId);
    console.log(`gateway ${gatewayId} approved`);

    await con.assign(gatewayId, { ruleset, allowlist, server, relayGroups: [groupId] });
    await con.deploy(gatewayId);
    console.log("deployed; waiting for the gateway to apply it and bind SMTP");

    // Tens of seconds, not milliseconds: the gateway's poll loop waits a
    // jittered 15s before its first /agent/status and registration does not
    // wake it, so this is the wait that used to be skipped.
    const ready = await waitForGatewayReady();
    console.log(`gateway is serving; the stack is ready (${describeReady(ready)})`);
}

await main();
