import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { checkSession } from "../src/middleware/checkSession.ts";
import {
    setSessionStore,
    type Session,
    type SessionStore,
} from "../src/globals.ts";

// The real store is DB-backed (Sessions table, migration 021). Swap in a plain
// Map so the auth gate can be exercised without a database.
const memory = new Map<string, Session>();
const memoryStore: SessionStore = {
    create: async (id, session) => {
        memory.set(id, session);
    },
    get: async (id) => {
        const session = memory.get(id);
        if (session && session.expiresAt <= Date.now()) {
            memory.delete(id);
            return undefined;
        }
        return session;
    },
    delete: async (id) => {
        memory.delete(id);
    },
    sweep: async () => {
        for (const [id, s] of memory) {
            if (s.expiresAt <= Date.now()) memory.delete(id);
        }
    },
};
setSessionStore(memoryStore);

beforeEach(() => memory.clear());

test("checkSession redirects when unauthenticated", async () => {
    let redirected = false;
    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify request stub
    const req: any = {
        cookies: {},
        // biome-ignore lint/suspicious/noExplicitAny: stub
        unsignCookie: (_: any) => ({ valid: false, value: null }),
        headers: {},
    };
    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify reply stub
    const reply: any = {
        redirect: (u: string) => {
            redirected = true;
            assert.strictEqual(u, "/login");
        },
    };

    await checkSession(req, reply);
    assert.ok(redirected, "should redirect to /login when no session");
});

test("checkSession allows a live session", async () => {
    const id = "sess-1";
    memory.set(id, { email: "a@b", expiresAt: Date.now() + 60_000 });

    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify request stub
    const req: any = {
        cookies: { session: id },
        // biome-ignore lint/suspicious/noExplicitAny: stub
        unsignCookie: (raw: any) => ({ valid: true, value: raw }),
        headers: {},
    };
    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify reply stub
    const reply: any = {
        redirect: (_: string) => {
            throw new Error("unexpected redirect");
        },
    };

    await checkSession(req, reply);
});

// The old in-memory store had no expiry at all; this is the regression guard.
test("checkSession rejects an expired session", async () => {
    const id = "sess-expired";
    memory.set(id, { email: "a@b", expiresAt: Date.now() - 1 });

    let redirected = false;
    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify request stub
    const req: any = {
        cookies: { session: id },
        // biome-ignore lint/suspicious/noExplicitAny: stub
        unsignCookie: (raw: any) => ({ valid: true, value: raw }),
        headers: {},
    };
    // biome-ignore lint/suspicious/noExplicitAny: minimal Fastify reply stub
    const reply: any = {
        redirect: (u: string) => {
            redirected = true;
            assert.strictEqual(u, "/login");
        },
    };

    await checkSession(req, reply);
    assert.ok(redirected, "an expired session must not authenticate");
    assert.equal(memory.has(id), false, "expired entry should be pruned");
});
