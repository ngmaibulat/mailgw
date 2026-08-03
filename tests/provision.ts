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
 * This script walks it the rest of the way, which is the same path an operator
 * walks on a real edge node:
 *
 *   1. create the console's first admin, and sign in
 *   2. create a relay group and a relay pointing at the mailhog sink
 *   3. create the three config profiles (ruleset, allowlist, server)
 *   4. wait for the gateway to register itself and approve its fingerprint
 *   5. assign the profiles and the relay group, then deploy
 *   6. wait until it is actually answering on SMTP
 *
 * It drives the console's HTML forms rather than an API, because the console
 * has no JSON admin API — /config/profiles/create and friends are form
 * handlers behind a cookie session. Seeding MariaDB directly was the
 * alternative and was rejected: composing a ConfigVersion by hand means
 * reimplementing bundle.ts's digest and shape rules in a second place, and the
 * moment those two disagree the e2e suite is testing a bundle no console would
 * ever produce.
 *
 * Every step is idempotent, so re-running against an already-provisioned stack
 * is a no-op and safe.
 *
 *   bun tests/provision.ts
 */

const CONSOLE = process.env.CONSOLE_URL ?? "https://localhost:4000";
const EMAIL = process.env.CONSOLE_EMAIL ?? "admin@lab.example";
const PASSWORD = process.env.CONSOLE_PASSWORD ?? "labpassword1";
const SMTP_PORT = Number(process.env.SMTP_PORT ?? "2525");
const RELAY_HOST = process.env.RELAY_HOST ?? "mailhog";
const RELAY_PORT = process.env.RELAY_PORT ?? "1025";

/**
 * The test build's control API, if one is running.
 *
 * Optional and probed rather than required: against the shipped image there is
 * no such endpoint and this script behaves exactly as it always has. Against
 * cmd/mailgw-go-test it closes the one gap that made a clean-volume run
 * impossible — see enrollGateway below.
 */
const TESTCTL = process.env.TESTCTL_URL ?? "http://127.0.0.1:9090";

/**
 * The console URL as the GATEWAY sees it, which is not the one this script
 * uses: the gateway dials it from inside the compose network, where "localhost"
 * is its own container.
 */
const GATEWAY_CENTRAL_URL =
    process.env.GATEWAY_CENTRAL_URL ?? "https://webui:4000";

// The console serves a self-signed pair in the dev stack.
process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

const PROFILE_RULESET = "e2e-rules";
const PROFILE_ALLOWLIST = "e2e-allowlist";
const PROFILE_SERVER = "e2e-server";
const RELAY_GROUP = "Outbound";

/**
 * The configuration the e2e suite runs against.
 *
 * allow_all is set because the gateway sees connections from the docker bridge
 * and from the host, and pinning those addresses would make the suite depend on
 * the runtime's networking. On a real node this would be a specific list.
 */
const RULESET = `version: 1
routes:
    - name: Default
      match: {always: true}
      then: [{action: relay, relay: ${RELAY_GROUP}}]
`;

const ALLOWLIST = `{"allowed": [], "allow_all": true}`;

const SERVER = `hostname: devbook.local
local_domains: [devbook.local, ngm.dev]
listen:
    - addr: "0.0.0.0:2525"
outbound:
    spool_dir: /opt/mailgw-go/queue
`;

let cookie = "";

async function req(
    path: string,
    init: RequestInit & { form?: Record<string, string | string[]> } = {},
): Promise<Response> {
    const headers = new Headers(init.headers);
    if (cookie) headers.set("cookie", cookie);

    let body: string | undefined;
    if (init.form) {
        const p = new URLSearchParams();
        for (const [k, v] of Object.entries(init.form)) {
            if (Array.isArray(v)) v.forEach((one) => p.append(k, one));
            else p.append(k, v);
        }
        body = p.toString();
        headers.set("content-type", "application/x-www-form-urlencoded");
    }

    const res = await fetch(`${CONSOLE}${path}`, {
        ...init,
        headers,
        body: body ?? init.body,
        redirect: "manual",
    });

    // Keep every cookie the console hands back; the session is the only state
    // this script carries between steps.
    const set = res.headers.getSetCookie?.() ?? [];
    if (set.length > 0) {
        cookie = set.map((c) => c.split(";")[0]).join("; ");
    }
    return res;
}

async function waitFor(
    what: string,
    fn: () => Promise<boolean>,
    { tries = 60, delayMs = 2000 } = {},
): Promise<void> {
    for (let i = 0; i < tries; i++) {
        try {
            if (await fn()) return;
        } catch {
            // Not up yet. The whole point of the retry.
        }
        await Bun.sleep(delayMs);
    }
    throw new Error(`timed out waiting for ${what}`);
}

async function signIn(): Promise<void> {
    await waitFor("the console to answer", async () => {
        const res = await req("/login");
        return res.status === 200 || res.status === 302;
    });

    // /setup creates the first admin and then closes permanently, so an
    // already-provisioned stack answers something other than 200/302 here and
    // that is not an error.
    await req("/setup", {
        method: "POST",
        form: { email: EMAIL, pass: PASSWORD, passConfirm: PASSWORD },
    });

    const res = await req("/login", {
        method: "POST",
        form: { email: EMAIL, pass: PASSWORD },
    });
    if (!cookie) {
        throw new Error(
            `sign-in did not yield a session cookie (status ${res.status})`,
        );
    }
}

/** ids are scraped from the list pages: there is no API that returns them. */
async function idFor(path: string, name: string): Promise<number | null> {
    const html = await (await req(path)).text();
    // Rows link to /<base>/edit/<id> or /<base>/<id>; find the id nearest the
    // name. Deliberately tolerant: this is a fixture bootstrap, not a parser.
    const rows = html.split("<tr>");
    for (const row of rows) {
        if (!row.includes(`>${name}<`)) continue;
        const m = row.match(/\/(?:edit\/)?(\d+)(?:["/])/);
        if (m) return Number(m[1]);
    }
    return null;
}

async function ensureRelayGroup(): Promise<number> {
    let id = await idFor("/config/relaygrp", RELAY_GROUP);
    if (id === null) {
        await req("/config/relaygrp/create", {
            method: "POST",
            form: { name: RELAY_GROUP },
        });
        id = await idFor("/config/relaygrp", RELAY_GROUP);
    }
    if (id === null) throw new Error("could not create the relay group");

    const groupPage = await (await req(`/config/relaygrp/edit/${id}`)).text();
    if (!groupPage.includes(RELAY_HOST)) {
        await req(`/config/relay/create/${id}`, {
            method: "POST",
            form: {
                name: "sink",
                host: RELAY_HOST,
                port: RELAY_PORT,
                priority: "0",
            },
        });
    }
    return id;
}

async function ensureProfile(
    name: string,
    kind: string,
    body: string,
): Promise<number> {
    let id = await idFor("/config/profiles", name);
    if (id === null) {
        await req("/config/profiles/create", {
            method: "POST",
            form: {
                kind,
                name,
                description: "created by tests/provision.ts",
                body,
            },
        });
        id = await idFor("/config/profiles", name);
    }
    if (id === null) throw new Error(`could not create the ${kind} profile`);
    return id;
}

/**
 * Point the gateway at the console, if it is a build that lets us.
 *
 * WHY THIS EXISTS
 *
 * A gateway registers itself only after somebody submits a Central Management
 * URL in its wizard, and since M12 that form is behind a session, which is
 * behind a claim code printed to the container log. Nothing automated it. So on
 * a FRESH data volume `docker compose down -v && pnpm provision` waited two
 * minutes for a registration that was never going to happen and threw.
 *
 * It went unnoticed because mailgw_go_data is a named volume: once somebody had
 * walked the wizard by hand, the identity and the URL survived every `down`
 * that omitted -v. A new clone, a CI runner and `down -v` all hit it.
 *
 * The test build exposes POST /testctl/enroll, which calls the same
 * agent.Register the wizard calls. Nothing else about provisioning changes: the
 * console still lands the row pending, and the fingerprint approval, the
 * assignment and the deploy below are all unchanged.
 *
 * Against the shipped image this probe just fails and we carry on waiting, so a
 * stack provisioned by hand still works.
 */
async function enrollGateway(): Promise<boolean> {
    let status: Response;
    try {
        status = await fetch(`${TESTCTL}/testctl/status`, {
            signal: AbortSignal.timeout(2000),
        });
    } catch {
        return false; // Not a test build, or not up yet.
    }
    if (!status.ok) return false;

    const before = (await status.json()) as { gateway_uid?: string };
    if (before.gateway_uid) {
        console.log("gateway is already enrolled");
        return true;
    }

    const resp = await fetch(`${TESTCTL}/testctl/enroll`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
            central_url: GATEWAY_CENTRAL_URL,
            // The console serves a self-signed pair in the dev stack.
            insecure_tls: true,
        }),
    });
    if (!resp.ok) {
        throw new Error(`enroll failed: ${resp.status} ${await resp.text()}`);
    }
    const after = (await resp.json()) as { fingerprint?: string };
    console.log(`enrolled via the test control API (${after.fingerprint})`);
    return true;
}

/**
 * The gateway registers itself, so this waits rather than creating anything.
 * Registration is open by design; what an operator approves is the fingerprint.
 */
async function approveGateway(): Promise<number> {
    let id = 0;
    await waitFor("the gateway to register itself", async () => {
        const html = await (await req("/gateways")).text();
        const m = html.match(/\/gateways\/(\d+)/);
        if (!m) return false;
        id = Number(m[1]);
        return true;
    });

    await req(`/gateways/${id}/status`, {
        method: "POST",
        form: { status: "approved" },
    });
    return id;
}

async function main(): Promise<void> {
    console.log(`console:  ${CONSOLE}`);
    await signIn();
    console.log("signed in");

    const groupId = await ensureRelayGroup();
    console.log(`relay group ${RELAY_GROUP} (${groupId}) -> ${RELAY_HOST}:${RELAY_PORT}`);

    const ruleset = await ensureProfile(PROFILE_RULESET, "ruleset", RULESET);
    const allowlist = await ensureProfile(
        PROFILE_ALLOWLIST,
        "allowlist",
        ALLOWLIST,
    );
    const server = await ensureProfile(PROFILE_SERVER, "server", SERVER);
    console.log(`profiles: ruleset=${ruleset} allowlist=${allowlist} server=${server}`);

    if (!(await enrollGateway())) {
        console.log(
            "no test control API at " +
                TESTCTL +
                "; waiting for the gateway to be pointed at the console by hand " +
                "(open its wizard on :8080 — the claim code is in its log)",
        );
    }

    const gatewayId = await approveGateway();
    console.log(`gateway ${gatewayId} approved`);

    await req(`/gateways/${gatewayId}/assignments`, {
        method: "POST",
        form: {
            profile_ruleset: String(ruleset),
            profile_allowlist: String(allowlist),
            profile_server: String(server),
            relay_groups: [String(groupId)],
        },
    });
    await req(`/gateways/${gatewayId}/deploy`, {
        method: "POST",
        form: { note: "e2e" },
    });
    console.log("deployed; waiting for the gateway to serve SMTP");

    await waitFor(`SMTP on :${SMTP_PORT}`, async () => {
        const sock = await Bun.connect({
            hostname: "127.0.0.1",
            port: SMTP_PORT,
            socket: { data() {}, error() {} },
        });
        sock.end();
        return true;
    });
    console.log("gateway is serving; the stack is ready");
}

await main();
