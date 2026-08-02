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
    gatewayAssignments,
} from "../db/index.ts";
import { setSessionStore, type Session, type SessionStore } from "./globals.ts";

// Transaction behaviour for the Central Management writes (M9.4).
//
// These assert that the handlers route their mutations through the transaction
// handle rather than the pool — which is the regression that actually matters,
// because the day someone writes `db.insert(...)` instead of `tx.insert(...)`
// inside the callback, the statement silently escapes the transaction and the
// partial-write bug is back with the code still *looking* correct.
//
// LIMIT, stated plainly: this proves our code uses the transaction handle. It
// does NOT prove MariaDB rolls back — the stub simulates that. The real
// rollback is covered by the opt-in DB-backed test noted at the end of this
// file.

interface Write {
    table: unknown;
    values?: unknown;
    inTx: boolean;
}

const tableRows = new Map<unknown, unknown[]>();
let writes: Write[] = [];
let txCalls = 0;
// Set by a test to make the next insert throw, simulating a mid-sequence
// failure.
let failNextInsertTo: unknown = null;

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

// One recorder factory, parameterised by whether it represents the pooled
// connection or a transaction handle. Both are handed to the same code path,
// so `inTx` is what distinguishes them.
function makeHandle(inTx: boolean) {
    return {
        select: () => makeSelectStub(),
        insert: (table: unknown) => ({
            values: (values: unknown) => {
                if (failNextInsertTo === table) {
                    failNextInsertTo = null;
                    return Promise.reject(
                        new Error("simulated insert failure"),
                    );
                }
                writes.push({ table, values, inTx });
                return Promise.resolve();
            },
        }),
        update: () => ({
            set: (values: unknown) => ({
                where: () => {
                    writes.push({ table: null, values, inTx });
                    return Promise.resolve();
                },
            }),
        }),
        delete: (table: unknown) => ({
            where: () => {
                writes.push({ table, inTx });
                return Promise.resolve();
            },
        }),
    };
}

const pooled = makeHandle(false);
const txHandle = makeHandle(true);

Object.assign(db, {
    select: pooled.select,
    insert: pooled.insert,
    update: pooled.update,
    delete: pooled.delete,
    $count: () => Promise.resolve(1),
    transaction: async (fn: (tx: unknown) => Promise<unknown>) => {
        txCalls++;
        const mark = writes.length;
        try {
            return await fn(txHandle);
        } catch (err) {
            // Simulate the rollback: discard everything the callback wrote.
            writes = writes.slice(0, mark);
            throw err;
        }
    },
});

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

function adminCookie(): string {
    const sid = "sess-admin";
    memorySessions.set(sid, {
        email: "admin@test",
        expiresAt: Date.now() + 60_000,
    });
    tableRows.set(users, [{ role: "admin" }]);
    return `session=${app.signCookie(sid)}`;
}

function writesTo(table: unknown): Write[] {
    return writes.filter((w) => w.table === table);
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
    writes = [];
    txCalls = 0;
    failNextInsertTo = null;
    tableRows.set(gateways, [
        { id: 1, gateway_uid: "uid-1", status: "pending" },
    ]);
});

describe("gateway assignments are transactional", () => {
    it("performs the delete and re-insert inside one transaction", async () => {
        tableRows.set(configProfiles, [{ id: 5, kind: "ruleset" }]);

        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/assignments",
            headers: { cookie: adminCookie() },
            payload: { profile_ruleset: "5", relay_groups: "3" },
        });

        assert.equal(res.statusCode, 302);
        assert.equal(txCalls, 1, "expected exactly one transaction");

        const assignmentWrites = writesTo(gatewayAssignments);
        assert.ok(assignmentWrites.length > 0, "nothing was written");
        // The load-bearing assertion.
        for (const w of assignmentWrites) {
            assert.equal(
                w.inTx,
                true,
                "an assignment write escaped the transaction",
            );
        }
    });

    it("collapses the inserts into a single multi-row insert", async () => {
        tableRows.set(configProfiles, [
            { id: 5, kind: "ruleset" },
            { id: 6, kind: "allowlist" },
        ]);

        await app.inject({
            method: "POST",
            url: "/gateways/1/assignments",
            headers: { cookie: adminCookie() },
            payload: {
                profile_ruleset: "5",
                profile_allowlist: "6",
                relay_groups: ["3", "4"],
            },
        });

        const inserts = writesTo(gatewayAssignments).filter((w) => w.values);
        assert.equal(inserts.length, 1, "expected one multi-row insert");
        assert.ok(
            Array.isArray(inserts[0].values),
            "expected the insert to carry an array of rows",
        );
        assert.equal((inserts[0].values as unknown[]).length, 4);
    });

    // The bug this milestone exists to close: a failure after the delete left
    // the gateway with no assignments at all, and the next Deploy froze that
    // emptiness as a real, rollback-able version.
    it("does not strand the gateway with no assignments when the insert fails", async () => {
        tableRows.set(configProfiles, [{ id: 5, kind: "ruleset" }]);
        failNextInsertTo = gatewayAssignments;

        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/assignments",
            headers: { cookie: adminCookie() },
            payload: { profile_ruleset: "5" },
        });

        assert.equal(res.statusCode, 400);
        assert.equal(
            writesTo(gatewayAssignments).length,
            0,
            "the delete survived a failed insert",
        );
    });

    it("rejects a profile whose kind does not match its slot", async () => {
        // A ruleset profile assigned to the `server` slot would land in the
        // bundle as server.yaml.
        tableRows.set(configProfiles, [{ id: 5, kind: "ruleset" }]);

        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/assignments",
            headers: { cookie: adminCookie() },
            payload: { profile_server: "5" },
        });

        assert.equal(res.statusCode, 400);
        assert.match(res.body, /cannot be assigned/);
        assert.equal(
            writesTo(gatewayAssignments).length,
            0,
            "a rejected assignment still wrote to the table",
        );
    });

    it("rejects an assignment naming a profile that does not exist", async () => {
        tableRows.set(configProfiles, []);

        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/assignments",
            headers: { cookie: adminCookie() },
            payload: { profile_ruleset: "999" },
        });

        assert.equal(res.statusCode, 400);
        assert.equal(writesTo(gatewayAssignments).length, 0);
    });
});

describe("forgetting a gateway is transactional", () => {
    it("deletes from all four tables inside one transaction", async () => {
        const res = await app.inject({
            method: "POST",
            url: "/gateways/1/delete",
            headers: { cookie: adminCookie() },
        });

        assert.equal(res.statusCode, 302);
        assert.equal(txCalls, 1);

        const deletes = writes.filter((w) => !w.values);
        assert.equal(deletes.length, 4, "expected four deletes");
        for (const w of deletes) {
            assert.equal(w.inTx, true, "a delete escaped the transaction");
        }
    });
});

describe("gateway ids are validated", () => {
    // `+id` on a non-numeric segment yields NaN, which flows into eq() and
    // silently matches nothing.
    it("rejects a non-numeric gateway id instead of querying for NaN", async () => {
        for (const url of [
            "/gateways/abc/assignments",
            "/gateways/abc/delete",
            "/gateways/abc/rename",
        ]) {
            const res = await app.inject({
                method: "POST",
                url,
                headers: { cookie: adminCookie() },
                payload: {},
            });
            assert.equal(res.statusCode, 400, `${url} should be a 400`);
        }
        // The audit-log hook writes a Logs row per request, which is expected;
        // what must not happen is a write to the gateway tables.
        assert.equal(
            writesTo(gatewayAssignments).length + writesTo(gateways).length,
            0,
            "a NaN id still caused a write to the gateway tables",
        );
        assert.equal(txCalls, 0, "a NaN id still opened a transaction");
    });
});

// Not covered here, deliberately: that MariaDB actually rolls back. The stub
// above simulates it. See webui-fastify/tests/central.tx.db.test.ts, which runs
// the same sequence against a real database and is opt-in via MAILGW_DB_CHECK
// because `node --test` must stay runnable without one.
