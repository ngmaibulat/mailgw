import assert from "node:assert/strict";
import { beforeEach, describe, it } from "node:test";

import { db } from "../../db/index.ts";
import { bundleDigest, composeBundle, serialiseBundle } from "./bundle.ts";

// composeBundle had no test before inbound AUTH added a key to it, and the
// invariant most at risk is the one stated three times in bundle.ts: an
// unchanged configuration must hash identically, or every gateway in the fleet
// re-pulls and restarts for a configuration that did not change.
//
// These tests stub the drizzle query builder rather than touching a database,
// the same way src/central.test.ts does — composeBundle takes its connection as
// a parameter precisely so this is possible.

type Row = Record<string, unknown>;

// tableRows is keyed by the drizzle table object identity, so "which gateway"
// and "which credential" stay distinguishable.
const tableRows = new Map<unknown, Row[]>();

interface SelectStub {
    from(table: unknown): SelectStub;
    where(..._args: unknown[]): SelectStub;
    orderBy(..._args: unknown[]): SelectStub;
    limit(_n: number): Promise<Row[]>;
    then<T>(resolve: (rows: Row[]) => T): Promise<T>;
}

function selectStub(): SelectStub {
    let rows: Row[] = [];
    const stub: SelectStub = {
        from(table) {
            rows = tableRows.get(table) ?? [];
            return stub;
        },
        where() {
            return stub;
        },
        orderBy() {
            return stub;
        },
        limit() {
            return Promise.resolve(rows);
        },
        // biome-ignore lint/suspicious/noThenProperty: drizzle query-builder stub
        then: (resolve) => Promise.resolve(resolve(rows)),
    };
    return stub;
}

// The connection composeBundle is handed. Only select() is reachable from it.
const conn = { select: () => selectStub() } as unknown as Parameters<
    typeof composeBundle
>[1];

// Imported lazily so the table identities match the ones bundle.ts uses.
const {
    gatewayAssignments,
    configProfiles,
    relayGroups,
    relays,
    smtpCredentials,
} = await import("../../db/index.ts");

function setRows(rows: {
    assignments?: Row[];
    profiles?: Row[];
    groups?: Row[];
    relays?: Row[];
    credentials?: Row[];
}) {
    tableRows.set(gatewayAssignments, rows.assignments ?? []);
    tableRows.set(configProfiles, rows.profiles ?? []);
    tableRows.set(relayGroups, rows.groups ?? []);
    tableRows.set(relays, rows.relays ?? []);
    tableRows.set(smtpCredentials, rows.credentials ?? []);
}

describe("composeBundle: inbound AUTH credentials", () => {
    beforeEach(() => {
        tableRows.clear();
        delete process.env.GATEWAY_METRICS_TOKEN;
    });

    it("omits the auth key entirely when no credential set is assigned", async () => {
        setRows({
            assignments: [
                { kind: "server", profile_id: 1, relay_group_id: null },
            ],
            profiles: [{ id: 1, kind: "server", body: "hostname: gw" }],
        });

        const bundle = await composeBundle(1, conn);

        // undefined, not {} and not {users: []}: stableStringify drops
        // undefined, and only that keeps an existing digest.
        assert.equal(bundle.auth, undefined);
        assert.ok(
            !serialiseBundle(bundle).includes('"auth"'),
            "an install with no credentials still serialises an auth key",
        );
    });

    it("keeps the digest of a configuration that has no credentials", async () => {
        const rows = {
            assignments: [
                { kind: "server", profile_id: 1, relay_group_id: null },
            ],
            profiles: [{ id: 1, kind: "server", body: "hostname: gw" }],
        };

        setRows(rows);
        const before = bundleDigest(
            serialiseBundle(await composeBundle(1, conn)),
        );

        // The same configuration, composed again after an unrelated credential
        // set exists in the database but is assigned to nobody.
        setRows({
            ...rows,
            credentials: [
                { id: 9, set_id: 9, username: "x", hash: "$2a$10$x" },
            ],
        });
        const after = bundleDigest(
            serialiseBundle(await composeBundle(1, conn)),
        );

        assert.equal(
            after,
            before,
            "a credential set assigned to another gateway changed this one's digest",
        );
    });

    it("carries assigned credentials as hashes", async () => {
        setRows({
            assignments: [{ kind: "credentialset", credential_set_id: 3 }],
            credentials: [
                {
                    id: 1,
                    set_id: 3,
                    username: "app@ngm.dev",
                    hash: "$2a$10$aaa",
                },
            ],
        });

        const bundle = await composeBundle(1, conn);

        assert.deepEqual(bundle.auth, {
            users: [{ user: "app@ngm.dev", hash: "$2a$10$aaa" }],
        });
    });

    it("emits credentials in a stable order whatever order the rows arrive in", async () => {
        const forward = [
            { id: 1, set_id: 3, username: "alice", hash: "$2a$10$a" },
            { id: 2, set_id: 3, username: "bob", hash: "$2a$10$b" },
        ];
        const assignments = [{ kind: "credentialset", credential_set_id: 3 }];

        setRows({ assignments, credentials: forward });
        const a = bundleDigest(serialiseBundle(await composeBundle(1, conn)));

        // stableStringify sorts object keys but deliberately not array
        // elements, so without an explicit sort the digest would follow
        // whatever order MySQL happened to return.
        setRows({ assignments, credentials: [...forward].reverse() });
        const b = bundleDigest(serialiseBundle(await composeBundle(1, conn)));

        assert.equal(a, b, "row order changed the bundle digest");
    });

    it("omits the auth key when the assigned set is empty", async () => {
        setRows({
            assignments: [{ kind: "credentialset", credential_set_id: 3 }],
            credentials: [],
        });

        const bundle = await composeBundle(1, conn);
        assert.equal(bundle.auth, undefined);
    });
});

// Guard against the regression src/central.tx.test.ts exists for: every query
// in composeBundle must go through the connection it was handed, or a deploy
// reads outside its own transaction.
describe("composeBundle: uses the connection it is given", () => {
    it("never falls back to the module-level db", async () => {
        tableRows.clear();
        setRows({
            assignments: [{ kind: "credentialset", credential_set_id: 3 }],
            credentials: [
                { id: 1, set_id: 3, username: "app", hash: "$2a$10$a" },
            ],
        });

        let usedGlobal = false;
        const original = db.select;
        (db as { select: unknown }).select = () => {
            usedGlobal = true;
            return selectStub();
        };
        try {
            await composeBundle(1, conn);
        } finally {
            (db as { select: unknown }).select = original;
        }

        assert.equal(
            usedGlobal,
            false,
            "composeBundle read through the global db",
        );
    });
});
