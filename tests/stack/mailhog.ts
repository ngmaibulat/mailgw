/**
 * Reading MailHog.
 *
 * MailHog is the one thing in this stack that neither Go nor the Tier-B fakes
 * can stand in for: a third-party MTA, written by somebody else, accepting what
 * this gateway produces. Everything else about delivery is better tested with
 * the scriptable sink in tests/harness — this exists for the assertion that a
 * real receiver takes the message.
 */

import { MAILHOG_URL } from "./baseline.ts";

export interface MailhogMessage {
    ID: string;
    Content: {
        Headers: Record<string, string[]>;
        Body: string;
    };
    Raw: { From: string; To: string[]; Data: string };
}

/** Every message currently held, newest first. */
export async function messages(limit = 200): Promise<MailhogMessage[]> {
    const res = await fetch(`${MAILHOG_URL}/api/v2/messages?limit=${limit}`, {
        signal: AbortSignal.timeout(10_000),
    });
    if (!res.ok) throw new Error(`MailHog /api/v2/messages -> ${res.status}`);
    const body = (await res.json()) as { items?: MailhogMessage[] };
    return body.items ?? [];
}

/**
 * Poll until a message matches.
 *
 * Every Tier-A test finds its own message by a unique subject rather than
 * clearing the inbox: the stack is shared, and a test that deleted everything
 * would break whatever ran beside it.
 */
export async function waitForMessage(
    pred: (m: MailhogMessage) => boolean,
    timeoutMs = 30_000,
): Promise<MailhogMessage> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
        const all = await messages().catch(() => [] as MailhogMessage[]);
        const hit = all.find(pred);
        if (hit) return hit;
        if (Date.now() > deadline) {
            throw new Error(
                `no MailHog message matched in ${timeoutMs}ms (${all.length} in the inbox)`,
            );
        }
        await Bun.sleep(500);
    }
}

/** A header value, case-insensitively, or "" when absent. */
export function header(m: MailhogMessage, name: string): string {
    const key = Object.keys(m.Content.Headers).find(
        (k) => k.toLowerCase() === name.toLowerCase(),
    );
    return key ? (m.Content.Headers[key]?.[0] ?? "") : "";
}

/** True when MailHog is reachable — used to skip rather than fail. */
export async function reachable(): Promise<boolean> {
    try {
        const res = await fetch(`${MAILHOG_URL}/api/v2/messages?limit=1`, {
            signal: AbortSignal.timeout(2000),
        });
        return res.ok;
    } catch {
        return false;
    }
}
