import { describe, it, expect } from "bun:test";
import {
    CONNECTION_FIELDS,
    DELIVERY_FIELDS,
    TRANSACTION_FIELDS,
} from "../src/query/search";
import { buildWhere } from "../src/query/builder";

// The failure this file exists to prevent: buildWhere SILENTLY SKIPS a field
// that is not in the table's allowlist (src/query/builder.ts). A column added
// to the schema and to a grid but forgotten here produces a filter that looks
// like it works, returns every row, and reports the same `total` either way —
// so nothing about the response says the filter was ignored.
describe("search field allowlists", () => {
    it("allows filtering log rows by gateway", () => {
        expect(CONNECTION_FIELDS.has("gateway")).toBe(true);
        expect(TRANSACTION_FIELDS.has("gateway")).toBe(true);
        expect(DELIVERY_FIELDS.has("gateway")).toBe(true);
    });

    it("allows filtering deliveries by route_rule", () => {
        expect(DELIVERY_FIELDS.has("route_rule")).toBe(true);
    });

    // route_rule is Delivery-only on purpose: routing is per recipient, and a
    // Transaction is one row per message, so the value there would be
    // ambiguous whenever two recipients were routed by different rules.
    it("does not offer route_rule on the message-level tables", () => {
        expect(TRANSACTION_FIELDS.has("route_rule")).toBe(false);
        expect(CONNECTION_FIELDS.has("route_rule")).toBe(false);
    });

    it("actually emits a WHERE clause for gateway", () => {
        const { sql, values } = buildWhere(
            [{ field: "gateway", operator: "is", value: "gw-1" }],
            "AND",
            DELIVERY_FIELDS,
        );
        expect(sql).toContain("gateway");
        expect(values).toContain("gw-1");
    });

    // The same call against a field nobody allowlisted, to show what the bug
    // above actually looks like: no clause, no error.
    it("silently drops an unknown field, which is why the checks above matter", () => {
        const { sql } = buildWhere(
            [{ field: "not_a_column", operator: "is", value: "x" }],
            "AND",
            DELIVERY_FIELDS,
        );
        expect(sql).toBe("");
    });
});
