import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";

process.env.SIGN_COOKIE = process.env.SIGN_COOKIE || "test-secret";

import type { FastifyInstance } from "fastify";

import { build } from "./app.ts";
import {
    db,
    users,
    gateways,
    configProfiles,
    configVersions,
    configDeployments,
    gatewayAssignments,
    relayGroups,
} from "../db/index.ts";
import { setSessionStore, type Session, type SessionStore } from "./globals.ts";

// Table-aware db stub (same approach as agent.test.ts): these routes touch
// several tables per request, so a single shared result would not distinguish
// "which user am I" from "which gateways exist".
const tableRows = new Map<unknown, unknown[]>();
const inserted: { table: unknown; values: unknown }[] = [];
const updated: { values: unknown }[] = [];
const deleted: unknown[] = [];

// The audit-log onRequest hook inserts a Logs row for every non-noise request,
// so "did the handler write anything?" has to be asked per table.
function insertsTo(table: unknown): unknown[] {
    return inserted.filter((i) => i.table === table).map((i) => i.values);
}

interface SelectStub extends PromiseLike<unknown[]> {
    from: (table: unknown) => SelectStub;
    where: () => SelectStub;
    orderBy: () => SelectStub;
    limit: () => Promise<unknown[]>;
}

function makeSelectStub(): SelectStub {
    let table: unknown;
    const rows = () => tableRows.get(table) ?? [];
    const stub: SelectStub = {
        from: (t: unknown) => {
            table = t;
            return stub;
        },
        where: () => stub,
        orderBy: () => stub,
        limit: () => Promise.resolve(rows()),
        // biome-ignore lint/suspicious/noThenProperty: drizzle query-builder stub
        then: (onF, onR) => Promise.resolve(rows()).then(onF, onR),
    };
    return stub;
}

(db as unknown as { select: () => SelectStub }).select = () => makeSelectStub();
(
    db as unknown as {
        insert: (t: unknown) => { values: (v: unknown) => Promise<void> };
    }
).insert = (table: unknown) => ({
    values: (values: unknown) => {
        inserted.push({ table, values });
        return Promise.resolve();
    },
});
(
    db as unknown as {
        update: () => { set: (v: unknown) => { where: () => Promise<void> } };
    }
).update = () => ({
    set: (values: unknown) => ({
        where: () => {
            updated.push({ values });
            return Promise.resolve();
        },
    }),
});
(
    db as unknown as {
        delete: (t: unknown) => { where: () => Promise<void> };
    }
).delete = (table: unknown) => ({
    where: () => {
        deleted.push(table);
        return Promise.resolve();
    },
});
(db as unknown as { $count: () => Promise<number> }).$count = () =>
    Promise.resolve(1);

const memorySessions = new Map<string, Session>();
const memoryStore: SessionStore = {
    create: async (id, s) => {
        memorySessions.set(id, s);
    },
    get: async (id) => memorySessions.get(id),
    delete: async (id) => {
        memorySessions.delete(id);
    },
    sweep: async () => {},
};
setSessionStore(memoryStore);

let app: FastifyInstance;

function cookieFor(role: "admin" | "viewer"): string {
    const sid = `sess-${role}`;
    memorySessions.set(sid, {
        email: `${role}@test`,
        expiresAt: Date.now() + 60_000,
    });
    // requireAdmin resolves the role through getUserRole -> Users.
    tableRows.set(users, [{ role }]);
    return `session=${app.signCookie(sid)}`;
}

before(async () => {
    app = await build({});
    await app.ready();
});

after(async () => {
    await app.close();
});

beforeEach(() => {
    tableRows.clear();
    inserted.length = 0;
    updated.length = 0;
    deleted.length = 0;
    tableRows.set(gateways, []);
    tableRows.set(configVersions, []);
    tableRows.set(configProfiles, []);
    tableRows.set(configDeployments, []);
    tableRows.set(gatewayAssignments, []);
    tableRows.set(relayGroups, []);
});

describe("gateway console auth", () => {
    it("redirects an unauthenticated browser away from /gateways", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/gateways",
            headers: { accept: "text/html" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
    });

    it("lets a viewer read the gateway list", async () => {
        const cookie = cookieFor("viewer");
        const res = await app.inject({
            method: "GET",
            url: "/gateways",
            headers: { cookie },
        });
        assert.equal(res.statusCode, 200);
    });

    // Approval is what lets a gateway pull relay credentials, so it must not be
    // something a read-only account can do.
    it("refuses a viewer approving a gateway", async () => {
        const cookie = cookieFor("viewer");
        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/status",
            headers: { cookie },
            payload: { status: "approved" },
        });

        assert.equal(res.statusCode, 403);
        assert.equal(updated.length, 0, "nothing may be written");
    });

    it("lets an admin approve a gateway and records who did it", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/status",
            headers: { cookie },
            payload: { status: "approved" },
        });

        assert.equal(res.statusCode, 302);
        const values = updated.at(-1)?.values as Record<string, unknown>;
        assert.equal(values.status, "approved");
        assert.equal(values.approved_by, "admin@test");
    });

    // "pending" is the state registration creates; nothing should be able to
    // push a gateway back into the approval queue by hand.
    it("rejects an unknown status value", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/status",
            headers: { cookie },
            payload: { status: "pending" },
        });

        assert.equal(res.statusCode, 400);
        assert.equal(updated.length, 0);
    });

    it("refuses a viewer deploying configuration", async () => {
        const cookie = cookieFor("viewer");
        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/deploy",
            headers: { cookie },
            payload: { note: "nope" },
        });
        assert.equal(res.statusCode, 403);
        assert.equal(insertsTo(configVersions).length, 0);
        assert.equal(insertsTo(configDeployments).length, 0);
    });
});

describe("config profiles", () => {
    it("replaces the old /config/routing stub with the profile list", async () => {
        const cookie = cookieFor("viewer");
        const res = await app.inject({
            method: "GET",
            url: "/config/routing",
            headers: { cookie },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/config/profiles");
    });

    it("refuses a viewer creating a profile", async () => {
        const cookie = cookieFor("viewer");
        const res = await app.inject({
            method: "POST",
            url: "/config/profiles/create",
            headers: { cookie },
            payload: { kind: "ruleset", name: "r", body: "version: 1" },
        });
        assert.equal(res.statusCode, 403);
        assert.equal(insertsTo(configProfiles).length, 0);
    });

    it("accepts a valid ruleset profile", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/config/profiles/create",
            headers: { cookie },
            payload: {
                kind: "ruleset",
                name: "corp-routing",
                description: "",
                body: "version: 1\nroutes: []\n",
            },
        });

        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/config/profiles");
        assert.equal(insertsTo(configProfiles).length, 1);
    });

    it("rejects an unknown profile kind (no mass-assignment of kinds)", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/config/profiles/create",
            headers: { cookie },
            payload: { kind: "wat", name: "x", body: "y" },
        });
        assert.equal(res.statusCode, 400);
        assert.equal(insertsTo(configProfiles).length, 0);
    });

    // The gateway is authoritative for the rule DSL, but an allowlist that
    // isn't even JSON would fail-close the gateway on deploy — catch it here.
    it("rejects an allowlist body that is not valid JSON", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/config/profiles/create",
            headers: { cookie },
            payload: { kind: "allowlist", name: "nets", body: "10.0.0.0/8" },
        });

        assert.equal(res.statusCode, 400);
        assert.match(res.body, /not valid JSON/);
        assert.equal(insertsTo(configProfiles).length, 0);
    });

    it("rejects an allowlist body missing the `allowed` array", async () => {
        const cookie = cookieFor("admin");
        const res = await app.inject({
            method: "POST",
            url: "/config/profiles/create",
            headers: { cookie },
            payload: {
                kind: "allowlist",
                name: "nets",
                body: '{"nets":["10.0.0.0/8"]}',
            },
        });

        assert.equal(res.statusCode, 400);
        assert.equal(insertsTo(configProfiles).length, 0);
    });
});
