/**
 * Driving the Central Management console.
 *
 * The console has no JSON admin API — /config/profiles/create and friends are
 * form handlers behind a cookie session — so this drives the HTML forms, which
 * is what an operator's browser does.
 *
 * Seeding MariaDB directly was the alternative and was rejected: composing a
 * ConfigVersion by hand means reimplementing bundle.ts's digest and shape rules
 * in a second place, and the moment those two disagree the e2e suite is testing
 * a bundle no console would ever produce.
 *
 * Extracted from tests/provision.ts so the script and the Tier-A tests share one
 * definition of "how you drive the console". provision.ts is now a main() over
 * this file.
 */

import {
    CONSOLE_EMAIL,
    CONSOLE_PASSWORD,
    CONSOLE_URL,
    GATEWAY_CENTRAL_URL,
    RELAY_HOST,
    RELAY_PORT,
    TESTCTL_URL,
} from "./baseline.ts";

// The console serves a self-signed pair in the dev stack.
process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

export class Console {
    private cookie = "";

    constructor(readonly baseUrl: string = CONSOLE_URL) {}

    /** One request, carrying the session and serialising a form body. */
    async req(
        path: string,
        init: RequestInit & { form?: Record<string, string | string[]> } = {},
    ): Promise<Response> {
        const headers = new Headers(init.headers);
        if (this.cookie) headers.set("cookie", this.cookie);

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

        const res = await fetch(`${this.baseUrl}${path}`, {
            ...init,
            headers,
            body: body ?? init.body,
            redirect: "manual",
        });

        // Keep every cookie the console hands back; the session is the only
        // state carried between steps.
        const set = res.headers.getSetCookie?.() ?? [];
        if (set.length > 0) {
            this.cookie = set.map((c) => c.split(";")[0]).join("; ");
        }
        return res;
    }

    async text(path: string): Promise<string> {
        return (await this.req(path)).text();
    }

    async signIn(): Promise<void> {
        await waitFor("the console to answer", async () => {
            const res = await this.req("/login");
            return res.status === 200 || res.status === 302;
        });

        // /setup creates the first admin and then closes permanently, so an
        // already-provisioned stack answers something else here and that is not
        // an error.
        await this.req("/setup", {
            method: "POST",
            form: {
                email: CONSOLE_EMAIL,
                pass: CONSOLE_PASSWORD,
                passConfirm: CONSOLE_PASSWORD,
            },
        });

        // Retried rather than fatal on the first miss. Two sign-ins racing each
        // other come back 302 with no Set-Cookie — which is why the suite uses
        // sharedConsole() and signs in once — but a retry costs nothing and
        // turns a whole file failing in beforeAll into a second attempt.
        let res: Response | undefined;
        for (let i = 0; i < 3 && !this.cookie; i++) {
            if (i > 0) await Bun.sleep(500);
            res = await this.req("/login", {
                method: "POST",
                form: { email: CONSOLE_EMAIL, pass: CONSOLE_PASSWORD },
            });
        }
        if (!this.cookie) {
            throw new Error(
                `sign-in did not yield a session cookie (status ${res?.status}); ` +
                    `is ${this.baseUrl} the console, and are the credentials right?`,
            );
        }
    }

    /** ids are scraped from the list pages: there is no API that returns them. */
    async idFor(path: string, name: string): Promise<number | null> {
        const html = await this.text(path);
        // Rows link to /<base>/edit/<id> or /<base>/<id>; find the id nearest
        // the name. Deliberately tolerant: this is a fixture bootstrap, not a
        // parser.
        for (const row of html.split("<tr>")) {
            if (!row.includes(`>${name}<`)) continue;
            const m = row.match(/\/(?:edit\/)?(\d+)(?:["/])/);
            if (m) return Number(m[1]);
        }
        return null;
    }

    /**
     * Create the relay group and the relay inside it, if they are not there.
     *
     * Two things here were wrong for a long time and are worth naming, because
     * between them they made `pnpm provision` report success while leaving a
     * gateway that could not relay at all:
     *
     *   - `group_id` has to be in the BODY. CtrlRelay.createHandle reads the
     *     path parameter only for the redirect and the error view; the value it
     *     validates comes from parseRelayBody, which is derived from the
     *     relays table and requires it. Without it the console answered
     *     400 "expected number, received NaN".
     *   - Nothing checked the status. A 400 here is invisible until the gateway
     *     refuses the deployed bundle with `relay group "Outbound" is empty`,
     *     which reads like a gateway problem and is not one.
     *
     * The existence check reads the group's DETAIL page, which is where the
     * relays are listed and where createHandle redirects. The edit page it used
     * to read renders the group's own form and never mentions a relay, so the
     * guard was answering a question it could not see.
     */
    async ensureRelayGroup(name: string): Promise<number> {
        let id = await this.idFor("/config/relaygrp", name);
        if (id === null) {
            await this.req("/config/relaygrp/create", { method: "POST", form: { name } });
            id = await this.idFor("/config/relaygrp", name);
        }
        if (id === null) throw new Error("could not create the relay group");

        if (!(await this.text(`/config/relaygrp/${id}`)).includes(RELAY_HOST)) {
            const res = await this.req(`/config/relay/create/${id}`, {
                method: "POST",
                form: {
                    group_id: String(id),
                    name: "sink",
                    host: RELAY_HOST,
                    port: RELAY_PORT,
                    priority: "0",
                },
            });
            if (res.status >= 400) {
                throw new Error(
                    `creating the relay in group ${id} failed: ${res.status} ` +
                        stripTags(await res.text()),
                );
            }
        }

        // Verified rather than assumed. An empty relay group is a configuration
        // the console will happily deploy and the gateway will always refuse.
        if (!(await this.text(`/config/relaygrp/${id}`)).includes(RELAY_HOST)) {
            throw new Error(
                `relay group ${name} (${id}) still has no relay pointing at ${RELAY_HOST}; ` +
                    "a gateway assigned this group will refuse every bundle",
            );
        }
        return id;
    }

    async ensureProfile(name: string, kind: string, body: string): Promise<number> {
        let id = await this.idFor("/config/profiles", name);
        if (id === null) {
            await this.req("/config/profiles/create", {
                method: "POST",
                form: {
                    kind,
                    name,
                    description: "created by tests/provision.ts",
                    body,
                },
            });
            id = await this.idFor("/config/profiles", name);
        }
        if (id === null) throw new Error(`could not create the ${kind} profile`);
        return id;
    }

    /** Replace a profile's body, which is how a Tier-A test changes config. */
    async updateProfile(id: number, kind: string, name: string, body: string): Promise<void> {
        await this.mutate(
            `/config/profiles/edit/${id}`,
            { kind, name, description: "updated by the e2e suite", body },
            `updating profile ${id}`,
        );
    }

    /** The gateway registers itself, so this waits rather than creating one. */
    async waitForGateway(): Promise<number> {
        let id = 0;
        await waitFor("the gateway to register itself", async () => {
            const html = await this.text("/gateways");
            const m = html.match(/\/gateways\/(\d+)/);
            if (!m) return false;
            id = Number(m[1]);
            return true;
        });
        return id;
    }

    /**
     * A console mutation that must succeed.
     *
     * Every one of these answers 302 on success and renders an HTML error view
     * on failure — `CtrlGateway.deploy` returns 400 with the compiler's
     * complaint, `adminOnly` returns 403 — so checking the status is the only
     * thing separating "it worked" from "it did not". `approve`, `assign` and
     * `deploy` checked nothing, which is the same omission that once let
     * `pnpm provision` report success while creating no relay at all; see
     * ensureRelayGroup. `rollback` deliberately does not come through here:
     * console.test.ts asserts on its status.
     */
    private async mutate(
        path: string,
        form: Record<string, string | string[]>,
        what: string,
    ): Promise<void> {
        const res = await this.req(path, { method: "POST", form });
        if (res.status >= 400) {
            throw new Error(`${what} failed: ${res.status} ${stripTags(await res.text())}`);
        }
    }

    async approve(id: number): Promise<void> {
        await this.mutate(
            `/gateways/${id}/status`,
            { status: "approved" },
            `approving gateway ${id}`,
        );
    }

    async assign(
        id: number,
        p: { ruleset: number; allowlist: number; server: number; relayGroups: number[] },
    ): Promise<void> {
        await this.mutate(
            `/gateways/${id}/assignments`,
            {
                profile_ruleset: String(p.ruleset),
                profile_allowlist: String(p.allowlist),
                profile_server: String(p.server),
                relay_groups: p.relayGroups.map(String),
            },
            `assigning profiles to gateway ${id}`,
        );
    }

    async deploy(id: number, note = "e2e"): Promise<void> {
        await this.mutate(`/gateways/${id}/deploy`, { note }, `deploying to gateway ${id}`);
    }

    async rollback(id: number, versionId: number): Promise<Response> {
        return this.req(`/gateways/${id}/rollback`, {
            method: "POST",
            form: { version_id: String(versionId) },
        });
    }

    /** The gateway detail page, for scraping the version table. */
    gatewayPage(id: number): Promise<string> {
        return this.text(`/gateways/${id}`);
    }
}

/**
 * One signed-in console for the whole suite.
 *
 * Memoised because a session is what an operator's browser has: one. Signing in
 * per test file mints a session per file and — since Bun imports every file
 * before running any of them — can put two sign-ins in flight at once, which the
 * console answers 302 with no Set-Cookie.
 */
let shared: Promise<Console> | null = null;

export function sharedConsole(): Promise<Console> {
    shared ??= (async () => {
        const c = new Console();
        await c.signIn();
        return c;
    })();
    return shared;
}

/**
 * Point the gateway at the console, if it is a build that lets us.
 *
 * A gateway registers itself only after somebody submits a Central Management
 * URL in its wizard, and since M12 that form is behind a session behind a claim
 * code printed to the container log. Nothing automated it, so on a FRESH data
 * volume `docker compose down -v && pnpm provision` waited two minutes for a
 * registration that was never going to happen.
 *
 * POST /testctl/enroll calls the same agent.Register the wizard calls. Against
 * the shipped image the probe simply fails and we carry on waiting, so a stack
 * provisioned by hand still works.
 */
export async function enrollGateway(): Promise<boolean> {
    // Retried, not probed once. `docker compose up -d` returns before the
    // gateway has opened its store and bound 9090, so a single two-second
    // attempt degrades silently into "waiting for a human to walk the wizard" —
    // which in CI means waiting 120s for something that is never going to
    // happen, and then failing somewhere else entirely.
    let status: Response | null = null;
    for (let i = 0; i < 15 && status === null; i++) {
        if (i > 0) await Bun.sleep(1000);
        try {
            const res = await fetch(`${TESTCTL_URL}/testctl/status`, {
                signal: AbortSignal.timeout(2000),
            });
            if (res.ok) status = res;
        } catch {
            // Not a test build, or not up yet. Try again.
        }
    }
    if (status === null) return false;

    // Enrolled unconditionally, rather than only when the status says it is not.
    //
    // Registration is idempotent per fingerprint on the console and can never
    // reset an approval, so re-enrolling costs one request. Skipping it on the
    // strength of the reported state does NOT cost nothing: Status is a cached
    // snapshot refreshed on a successful poll, and a gateway that cannot poll
    // is exactly the one that needs enrolling — after `POST /testctl/reset`,
    // for instance, it still reports the console URL it no longer has. The
    // check that looked like a saving was the thing that made the state
    // unrecoverable.
    const before = (await status.json()) as { gateway_uid?: string };
    if (before.gateway_uid) console.log("gateway is already registered; re-enrolling is a no-op");

    const resp = await fetch(`${TESTCTL_URL}/testctl/enroll`, {
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
 * True when the engineering build's control API is answering.
 *
 * Retried for the same reason `enrollGateway` is: this decides at IMPORT time
 * whether a whole Tier-A file runs, and Bun imports every file before running
 * any of them — so one slow answer during that burst used to skip the entire
 * configuration story while the job still exited 0.
 */
export async function haveTestctl(): Promise<boolean> {
    for (let i = 0; i < 5; i++) {
        if (i > 0) await Bun.sleep(1000);
        try {
            const res = await fetch(`${TESTCTL_URL}/testctl/status`, {
                signal: AbortSignal.timeout(2000),
            });
            if (res.ok) return true;
        } catch {
            // Not a test build, or not up yet.
        }
    }
    return false;
}

/** Enough of the HTML stripped that a console error is readable in a throw. */
function stripTags(html: string): string {
    return html
        .replace(/<[^>]+>/g, " ")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 300);
}

export async function waitFor(
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
