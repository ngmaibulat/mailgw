import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { expectedTables } from "../db/index.ts";

// The console blocks at boot until every table db/schema.ts declares exists
// (db/index.ts#waitForSchema), which is what replaced the db-migrator compose
// service in M22. That gate is only as good as this list, and the list is
// derived from the drizzle tables rather than written out — so what is worth
// pinning is that the derivation still finds them.
//
// No database: expectedTables() reads the schema module, and the pool in
// db/index.ts is lazy.
describe("expectedTables", () => {
    const tables = expectedTables();

    it("finds every table the console reads", () => {
        // A representative slice: the login path, Central Management, and the
        // audit trail. If drizzle ever changes how tables are branded, this
        // fails here rather than as a console that waits 90s and exits.
        for (const name of [
            "Users",
            "Sessions",
            "Relays",
            "RelayGroups",
            "Gateways",
            "ConfigProfiles",
            "ConfigVersions",
            "Logs",
        ]) {
            assert.ok(
                tables.includes(name),
                `${name} missing from ${tables.join(", ")}`,
            );
        }
    });

    it("returns the SQL names, sorted and non-empty", () => {
        assert.ok(tables.length >= 8);
        assert.deepEqual(tables, [...tables].sort());
        for (const name of tables) {
            assert.ok(name.length > 0, "a table resolved to an empty name");
        }
    });
});
