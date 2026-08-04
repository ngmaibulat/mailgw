/**
 * One definition of "the stack is ready".
 *
 * # Why this exists
 *
 * `tests/provision.ts` used to decide the gateway was serving by opening a TCP
 * connection to 127.0.0.1:2525 and closing it again. That is not a readiness
 * signal: `docker-compose.yaml` publishes 2525 from the moment the CONTAINER
 * starts, so with Docker's userland proxy the host-side accept succeeds whether
 * or not the process inside has bound anything — and nothing read the 220. The
 * predicate was therefore satisfied within a millisecond of the deploy, and
 * `pnpm provision` printed "the stack is ready" while the gateway was still
 * `pending`, holding no bundle and listening on nothing.
 *
 * That mattered because the gateway needs real time here, by design:
 * `internal/node/agent.go`'s poll loop waits a jittered 15s BEFORE its first
 * `/agent/status`, registration does not wake it, and `internal/central`'s
 * WebSocket fires its callback only on a received frame — never on connect — so
 * an approval or a deploy that lands while that socket is down is recovered
 * only by the next poll. Registering to applied is up to ~30s.
 *
 * So the whole Tier-A suite was racing a gateway that had not been configured
 * yet, and the failure surfaced three files later as `connection closed by
 * peer` — docker-proxy accepting and then finding nothing to forward to.
 *
 * # What "ready" means here
 *
 * Provisioned, approved, holding a CONSOLE-issued configuration, SMTP bound,
 * and answering 220 on it. Nothing weaker: each of those was, at some point,
 * the thing that was actually missing.
 *
 * # Two routes, because the shipped image has no control API
 *
 * With `docker-compose.test.yaml` the engineering build answers on 9090 and
 * reports all four facts directly. Without it — a stack brought up by hand from
 * `docker-compose.yaml` alone — the gateway's own `/readyz` answers the same
 * question, and answers it OPEN because the dev stack deploys no
 * `admin.metrics_token`. Both then have to pass the same SMTP check, which is
 * the one the old code only pretended to make.
 */

import { SmtpClient } from "../harness/smtp.ts";
import { Testctl, type Status } from "../harness/testctl.ts";
import { GATEWAY_ADMIN_URL, SMTP_HOST, SMTP_PORT, TESTCTL_URL } from "./baseline.ts";

export interface Ready {
    /** Which signal answered — useful in a CI log, and nothing else. */
    via: "testctl" | "readyz";
    /** The applied ConfigVersions id. Only the control API can report it. */
    versionId?: number;
    /** The addresses SMTP actually bound. Likewise. */
    listeners?: string[];
    /** The 220 line, proving something is really speaking SMTP. */
    greeting: string;
}

/**
 * Two poll intervals plus an apply and a bind, with slack. Deliberately longer
 * than any single wait inside the gateway, because the point of this helper is
 * that its timeout means "it is not coming", not "I did not wait long enough".
 */
const DEFAULT_TIMEOUT_MS = 90_000;

const POLL_MS = 500;

/** Thrown with the gateway's OWN account of what is missing. */
export class NotReadyError extends Error {
    constructor(
        readonly via: Ready["via"] | "none",
        readonly reasons: string[],
    ) {
        super(
            `the gateway never became ready (via ${via}): ` +
                (reasons.length > 0 ? reasons.join("; ") : "no reason reported"),
        );
        this.name = "NotReadyError";
    }
}

/**
 * Block until the gateway is genuinely serving a console-issued configuration.
 *
 * Deliberately NOT memoised. It is called from every Tier-A `beforeAll`, and
 * the one writer among them (`console.test.ts`) redeploys and rolls back — so
 * a cached "yes" from twenty seconds ago is exactly the answer that would let
 * the next file connect while listeners are being re-applied. Re-checking costs
 * one HTTP request and one SMTP connection.
 */
export async function waitForGatewayReady(opts: { timeoutMs?: number } = {}): Promise<Ready> {
    const deadline = Date.now() + (opts.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const ctl = new Testctl(TESTCTL_URL);

    let lastVia: Ready["via"] | "none" = "none";
    let reasons: string[] = [`nothing answered on ${TESTCTL_URL} or ${GATEWAY_ADMIN_URL}`];

    for (;;) {
        // The control API first: it is the only source that can distinguish a
        // console-issued version from an injected one, and the only one that
        // reports which addresses actually bound.
        let via: Ready["via"] | null = null;
        let versionId: number | undefined;
        let listeners: string[] | undefined;

        const st = await status(ctl);
        if (st) {
            via = "testctl";
            versionId = st.applied_version_id;
            listeners = st.listeners ?? undefined;
            reasons = missingFromStatus(st);
        } else {
            const rz = await readyz();
            if (rz) {
                via = "readyz";
                reasons = rz.reasons;
            }
        }
        if (via) lastVia = via;

        if (via && reasons.length === 0) {
            // Only now is it worth dialling SMTP. Doing it first would mean
            // asking a port that is published-but-unbound, which is precisely
            // the question this file exists to stop anyone asking.
            const greeting = await smtpGreeting();
            if (typeof greeting === "string") {
                return { via, versionId, listeners, greeting };
            }
            reasons = [greeting.err];
        }

        if (Date.now() > deadline) throw new NotReadyError(lastVia, reasons);
        await Bun.sleep(POLL_MS);
    }
}

/** The control API's answer, or null when this is not the engineering build. */
async function status(ctl: Testctl): Promise<Status | null> {
    try {
        return await ctl.status();
    } catch {
        return null;
    }
}

/**
 * What is still missing, in the gateway's own vocabulary.
 *
 * `serving` is deliberately NOT the listener check. `internal/node/gateway.go`
 * sets `g.live` before it binds, and a bind failure does not unset it, so
 * `serving: true` with `listeners: []` is reachable — which is also why
 * `/readyz`, which reads `serving`, cannot be the last word either and why the
 * SMTP greeting is checked on both routes.
 *
 * `apply_error` is carried as DIAGNOSIS and never blocks. `console.test.ts`
 * deliberately deploys a configuration the gateway must refuse and asserts that
 * it keeps serving the last good one — so a blocking check here would declare
 * the stack unready for every file that runs afterwards, which is the opposite
 * of what that test proves.
 */
function missingFromStatus(st: Status): string[] {
    const out: string[] = [];
    if (!st.provisioned) out.push("not provisioned");
    if (st.approval !== "approved") out.push(`not approved (${st.approval || "no answer yet"})`);
    // A NEGATIVE id is a testctl-injected bundle. It is a perfectly good
    // configuration but it did not come from the console, so it must not
    // satisfy a gate whose whole job is to prove that provisioning worked.
    if (st.applied_version_id <= 0) out.push("no console configuration applied");
    if (!st.listeners || st.listeners.length === 0) out.push("SMTP is not listening");
    if (out.length > 0) {
        if (st.apply_error) out.push(`apply_error=${st.apply_error}`);
        if (st.last_error) out.push(`last_error=${st.last_error}`);
    }
    return out;
}

/** The shipped image's answer: 200, or 503 with reasons. Null when unreachable. */
async function readyz(): Promise<{ reasons: string[] } | null> {
    try {
        const res = await fetch(`${GATEWAY_ADMIN_URL}/readyz`, {
            signal: AbortSignal.timeout(5000),
        });
        if (res.ok) return { reasons: [] };
        const body = (await res.json().catch(() => null)) as { reasons?: string[] } | null;
        return { reasons: body?.reasons ?? [`/readyz answered ${res.status}`] };
    } catch {
        return null;
    }
}

/** The 220 line, or the reason there wasn't one. */
async function smtpGreeting(): Promise<string | { err: string }> {
    let client: SmtpClient | undefined;
    try {
        client = await SmtpClient.open(SMTP_HOST, SMTP_PORT);
        const reply = await client.greeting(10_000);
        if (reply.code !== 220) {
            return { err: `SMTP on ${SMTP_HOST}:${SMTP_PORT} answered ${reply.code}, not 220` };
        }
        return reply.raw.trim();
    } catch (e) {
        return { err: `SMTP on ${SMTP_HOST}:${SMTP_PORT}: ${(e as Error).message}` };
    } finally {
        try {
            await client?.quit();
        } catch {
            /* the connection is going away either way */
        }
    }
}

/**
 * Whether an unusable stack is a failure rather than a skip.
 *
 * The mirror of MAILGW_REQUIRE_TIER_B, and for the same reason: a tier that can
 * silently skip itself in CI is a tier that will one day be skipped entirely
 * and nobody will notice. `haveTestctl()` is a two-second probe evaluated while
 * Bun is still importing files, so a slow answer used to skip the whole Tier-A
 * configuration story and leave the job exiting 0 with nothing asserted.
 */
export const REQUIRE_TIER_A = process.env.MAILGW_REQUIRE_TIER_A === "1";

/**
 * Decide, at import time, whether a Tier-A file should run.
 *
 * Returns true to skip. Under MAILGW_REQUIRE_TIER_A it throws instead, which
 * Bun reports as a failing file rather than a quiet absence.
 */
export function skipTierA(file: string, why: string, remedy: string): boolean {
    if (REQUIRE_TIER_A) {
        throw new Error(`${file}: ${why} (MAILGW_REQUIRE_TIER_A is set, so this is a failure)`);
    }
    console.error(`\n[stack] skipping ${file}: ${why}.\n  ${remedy}\n`);
    return true;
}

/** A one-line summary for a CI log. */
export function describeReady(r: Ready): string {
    const bits = [`via ${r.via}`];
    if (r.versionId !== undefined) bits.push(`version ${r.versionId}`);
    if (r.listeners && r.listeners.length > 0) bits.push(`listening on ${r.listeners.join(", ")}`);
    bits.push(`greeting ${JSON.stringify(r.greeting)}`);
    return bits.join("; ");
}
